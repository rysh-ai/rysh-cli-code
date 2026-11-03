// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
)

// Request headers. The GitHub names are accepted as aliases, so a repository
// webhook can point at a trigger unchanged.
const (
	SignatureHeader       = "X-Rysh-Signature-256"
	DeliveryHeader        = "X-Rysh-Delivery"
	GitHubSignatureHeader = "X-Hub-Signature-256"
	GitHubDeliveryHeader  = "X-GitHub-Delivery"
)

// MaxBodyBytes caps a webhook body. The body is only signed and counted, never
// passed on, so there is no reason to accept more.
const MaxBodyBytes = 64 << 10

// DefaultMinInterval is the shortest gap between two fires of one trigger.
const DefaultMinInterval = 5 * time.Second

// DefaultPort is the loopback port ##trigger listen binds when none is given.
// WhatsApp takes 23310, Telegram 23311 and phone 23312.
const DefaultPort = 23320

// FireRequest asks the workspace to fire one trigger.
type FireRequest struct {
	Name       string
	DeliveryID string
	BodyBytes  int
	// Skip, when set, asks the workspace to record a skipped run with this
	// reason and route nothing (the request was rate-limited).
	Skip string
}

// FireResult is the workspace's answer: a cron run status and a note.
type FireResult struct {
	Status string
	Note   string
}

// Handler serves POST /t/<name>. It authenticates the request against the
// trigger's secret and hands the fire to Fire; it never touches workspace
// state itself.
type Handler struct {
	// Secret returns a trigger's secret; false for an unknown trigger.
	Secret func(name string) ([]byte, bool)
	// Fire runs on the workspace side and returns the recorded outcome.
	Fire func(FireRequest) FireResult
	// Now is the clock for the rate limit (nil: time.Now).
	Now func() time.Time
	// MinInterval between fires of one trigger (0: DefaultMinInterval).
	MinInterval time.Duration

	mu         sync.Mutex
	lastFire   map[string]time.Time
	deliveries map[string]*deliveryRing
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) minInterval() time.Duration {
	if h.MinInterval > 0 {
		return h.MinInterval
	}
	return DefaultMinInterval
}

func writeJSON(w http.ResponseWriter, code int, status, note string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "note": note})
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/t/")
	if !ok || !ValidName(name) || r.URL.RawPath != "" {
		writeJSON(w, http.StatusNotFound, "not_found", "POST /t/<trigger-name>")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body over %d bytes", MaxBodyBytes))
			return
		}
		writeJSON(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}

	sig := r.Header.Get(SignatureHeader)
	if sig == "" {
		sig = r.Header.Get(GitHubSignatureHeader)
	}
	secret, known := h.Secret(name)
	// Unknown and unauthenticated answer the same, so names cannot be probed.
	if !known || !Verify(secret, body, sig) {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", "unknown trigger or bad signature")
		return
	}

	delivery := r.Header.Get(DeliveryHeader)
	if delivery == "" {
		delivery = r.Header.Get(GitHubDeliveryHeader)
	}
	req := FireRequest{Name: name, DeliveryID: delivery, BodyBytes: len(body)}

	// Reserve the delivery id and the rate slot under the lock BEFORE Fire, so
	// two concurrent requests cannot both pass the checks. A fire that did not
	// happen (blocked, skipped, error) gives the reservation back, so a
	// redelivery after the cause is fixed is not refused.
	h.mu.Lock()
	if h.lastFire == nil {
		h.lastFire = map[string]time.Time{}
		h.deliveries = map[string]*deliveryRing{}
	}
	ring := h.deliveries[name]
	if ring == nil {
		ring = &deliveryRing{}
		h.deliveries[name] = ring
	}
	if delivery != "" && ring.seen(delivery) {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, "duplicate", "delivery already handled")
		return
	}
	now := h.now()
	prev, hadPrev := h.lastFire[name]
	limited := hadPrev && now.Sub(prev) < h.minInterval()
	if limited {
		req.Skip = fmt.Sprintf("rate limited: under %s since the last fire", h.minInterval())
	} else {
		h.lastFire[name] = now
		if delivery != "" {
			ring.add(delivery)
		}
	}
	h.mu.Unlock()

	res := h.Fire(req)

	if limited {
		writeJSON(w, http.StatusTooManyRequests, cron.StatusSkipped, req.Skip)
		return
	}
	if res.Status != cron.StatusOK {
		h.mu.Lock()
		if h.lastFire[name].Equal(now) {
			if hadPrev {
				h.lastFire[name] = prev
			} else {
				delete(h.lastFire, name)
			}
		}
		if delivery != "" {
			ring.remove(delivery)
		}
		h.mu.Unlock()
	}
	switch res.Status {
	case cron.StatusOK:
		writeJSON(w, http.StatusAccepted, res.Status, res.Note)
	case cron.StatusBlocked:
		writeJSON(w, http.StatusServiceUnavailable, res.Status, res.Note)
	case cron.StatusSkipped:
		writeJSON(w, http.StatusConflict, res.Status, res.Note)
	default:
		writeJSON(w, http.StatusInternalServerError, cron.StatusError, res.Note)
	}
}

// ---------------------------------------------------------------------------
// Loopback listener
// ---------------------------------------------------------------------------

// Server is a running trigger listener.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Addr is the bound host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the listener.
func (s *Server) Close() error { return s.srv.Close() }

// LoopbackAddr validates addr as a loopback host:port without any DNS lookup:
// the host must be a loopback IP literal or "localhost" (read as 127.0.0.1).
// Reaching a trigger from outside is the operator's own tunnel.
func LoopbackAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("refusing to listen on %q: triggers bind loopback only (127.0.0.1 or ::1); expose them through your own tunnel", addr)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// Listen serves h on a loopback address.
func Listen(addr string, h http.Handler) (*Server, error) {
	addr, err := LoopbackAddr(addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot bind %s: %w", addr, err)
	}
	s := &Server{ln: ln, srv: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}
