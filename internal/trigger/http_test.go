// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
)

// fakeFirer records every fire and answers with a fixed result.
type fakeFirer struct {
	mu     sync.Mutex
	calls  []FireRequest
	result FireResult
}

func (f *fakeFirer) fire(r FireRequest) FireResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	if r.Skip != "" {
		return FireResult{Status: cron.StatusSkipped, Note: r.Skip}
	}
	return f.result
}

func (f *fakeFirer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type handlerFixture struct {
	h      *Handler
	firer  *fakeFirer
	secret []byte
	now    time.Time
}

func newFixture(t *testing.T) *handlerFixture {
	t.Helper()
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	f := &handlerFixture{firer: &fakeFirer{result: FireResult{Status: cron.StatusOK, Note: "dispatched"}}, secret: secret,
		now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	f.h = &Handler{
		Secret: func(name string) ([]byte, bool) {
			if name == "gh-push" {
				return f.secret, true
			}
			return nil, false
		},
		Fire: f.firer.fire,
		Now:  func() time.Time { return f.now },
	}
	return f
}

func (f *handlerFixture) post(t *testing.T, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *handlerFixture) signed(body []byte) map[string]string {
	return map[string]string{SignatureHeader: Sign(f.secret, body)}
}

func TestHandlerFiresOnGoodSignature(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"event":"push"}`)
	hdr := f.signed(body)
	hdr[DeliveryHeader] = "d-1"
	rec := f.post(t, "/t/gh-push", body, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body %q", rec.Code, rec.Body.String())
	}
	if f.firer.count() != 1 {
		t.Fatalf("fires = %d, want 1", f.firer.count())
	}
	got := f.firer.calls[0]
	if got.Name != "gh-push" || got.DeliveryID != "d-1" || got.BodyBytes != len(body) || got.Skip != "" {
		t.Fatalf("fire request = %+v", got)
	}
}

func TestHandlerAcceptsGitHubSignatureHeader(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"zen":"x"}`)
	rec := f.post(t, "/t/gh-push", body, map[string]string{
		GitHubSignatureHeader: Sign(f.secret, body), GitHubDeliveryHeader: "gh-d-1"})
	if rec.Code != http.StatusAccepted || f.firer.count() != 1 {
		t.Fatalf("code = %d, fires = %d; want 202 and 1", rec.Code, f.firer.count())
	}
	if f.firer.calls[0].DeliveryID != "gh-d-1" {
		t.Fatalf("GitHub delivery id not used: %+v", f.firer.calls[0])
	}
}

// Unknown names and bad signatures look identical from outside, so a caller
// cannot enumerate trigger names. Neither fires.
func TestHandlerRejectsUnknownAndBadSignatureAlike(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{}`)
	unknown := f.post(t, "/t/nope", body, f.signed(body))
	badSig := f.post(t, "/t/gh-push", body, map[string]string{SignatureHeader: "sha256=00"})
	noSig := f.post(t, "/t/gh-push", body, nil)
	for name, rec := range map[string]*httptest.ResponseRecorder{"unknown": unknown, "bad sig": badSig, "no sig": noSig} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code = %d, want 401", name, rec.Code)
		}
	}
	if unknown.Body.String() != badSig.Body.String() {
		t.Errorf("unknown and bad-signature bodies differ: %q vs %q", unknown.Body.String(), badSig.Body.String())
	}
	if f.firer.count() != 0 {
		t.Fatalf("rejected requests fired %d time(s)", f.firer.count())
	}
}

func TestHandlerRejectsPathShapedNames(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{}`)
	for _, p := range []string{"/t/../gh-push", "/t/gh-push/x", "/t/", "/gh-push", "/t/gh%2Fpush"} {
		rec := f.post(t, p, body, f.signed(body))
		if rec.Code == http.StatusAccepted {
			t.Errorf("%s fired", p)
		}
	}
	if f.firer.count() != 0 {
		t.Fatalf("path-shaped requests fired %d time(s)", f.firer.count())
	}
}

func TestHandlerMethodAndSize(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/t/gh-push", nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rec.Code)
	}
	big := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
	if rec := f.post(t, "/t/gh-push", big, f.signed(big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized code = %d, want 413", rec.Code)
	}
	if f.firer.count() != 0 {
		t.Fatalf("rejected requests fired %d time(s)", f.firer.count())
	}
}

// A redelivery (same delivery id) answers 200 and fires nothing.
func TestHandlerDedupesDeliveries(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{}`)
	hdr := f.signed(body)
	hdr[DeliveryHeader] = "same"
	if rec := f.post(t, "/t/gh-push", body, hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery code = %d", rec.Code)
	}
	f.now = f.now.Add(time.Hour) // well past the rate limit
	rec := f.post(t, "/t/gh-push", body, hdr)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("redelivery code = %d body %q, want 200 duplicate", rec.Code, rec.Body.String())
	}
	if f.firer.count() != 1 {
		t.Fatalf("fires = %d, want 1", f.firer.count())
	}
}

// Fires closer together than MinInterval are recorded as skipped (the actor
// sees a Skip request and routes nothing) and answered 429.
func TestHandlerRateLimitsOnTheFakeClock(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{}`)
	if rec := f.post(t, "/t/gh-push", body, f.signed(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("first code = %d", rec.Code)
	}
	f.now = f.now.Add(DefaultMinInterval - time.Second)
	rec := f.post(t, "/t/gh-push", body, f.signed(body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("early code = %d, want 429", rec.Code)
	}
	if f.firer.count() != 2 || f.firer.calls[1].Skip == "" {
		t.Fatalf("rate-limited fire not passed as a skip: %+v", f.firer.calls)
	}
	f.now = f.now.Add(time.Second)
	if rec := f.post(t, "/t/gh-push", body, f.signed(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("on-time code = %d, want 202", rec.Code)
	}
}

func TestHandlerMapsFireResults(t *testing.T) {
	cases := map[string]int{
		cron.StatusBlocked: http.StatusServiceUnavailable,
		cron.StatusSkipped: http.StatusConflict, // disabled trigger
		cron.StatusError:   http.StatusInternalServerError,
	}
	for status, want := range cases {
		f := newFixture(t)
		f.firer.result = FireResult{Status: status, Note: "n"}
		body := []byte(`{}`)
		rec := f.post(t, "/t/gh-push", body, f.signed(body))
		if rec.Code != want {
			t.Errorf("status %q: code = %d, want %d", status, rec.Code, want)
		}
	}
}

// A blocked or failed fire does not consume the rate-limit window: the next
// attempt after the policy is fixed is not refused as too early.
func TestHandlerOnlyOKFiresStartTheWindow(t *testing.T) {
	f := newFixture(t)
	f.firer.result = FireResult{Status: cron.StatusBlocked}
	body := []byte(`{}`)
	f.post(t, "/t/gh-push", body, f.signed(body))
	f.firer.result = FireResult{Status: cron.StatusOK}
	if rec := f.post(t, "/t/gh-push", body, f.signed(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("code after a blocked fire = %d, want 202", rec.Code)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "[::]:0", "example.com:0"} {
		if s, err := Listen(addr, http.NotFoundHandler()); err == nil {
			s.Close()
			t.Errorf("Listen(%q) bound a non-loopback address", addr)
		}
	}
}

// End to end over a real loopback socket.
func TestListenServesOnLoopback(t *testing.T) {
	f := newFixture(t)
	s, err := Listen("127.0.0.1:0", f.h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	body := []byte(`{"n":1}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/t/gh-push", bytes.NewReader(body))
	req.Header.Set(SignatureHeader, Sign(f.secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted || f.firer.count() != 1 {
		t.Fatalf("code = %d, fires = %d; want 202 and 1", resp.StatusCode, f.firer.count())
	}
}

// blockingFirer holds every fire until released, so two requests overlap.
type blockingFirer struct {
	fakeFirer
	entered chan struct{}
	release chan struct{}
}

func (b *blockingFirer) fire(r FireRequest) FireResult {
	if r.Skip == "" {
		b.entered <- struct{}{}
		<-b.release
	}
	return b.fakeFirer.fire(r)
}

func concurrentPosts(t *testing.T, f *handlerFixture, b *blockingFirer, hdrs ...map[string]string) []int {
	t.Helper()
	body := []byte(`{}`)
	codes := make([]int, len(hdrs))
	var wg sync.WaitGroup
	for i, h := range hdrs {
		wg.Add(1)
		go func(i int, h map[string]string) {
			defer wg.Done()
			codes[i] = f.post(t, "/t/gh-push", body, h).Code
		}(i, h)
	}
	<-b.entered // the first fire is in flight
	time.Sleep(200 * time.Millisecond)
	close(b.release)
	wg.Wait()
	return codes
}

// LEAD-CHANGES (2): the delivery id is reserved before Fire, so two
// concurrent POSTs carrying one id fire once.
func TestHandlerConcurrentSameDeliveryFiresOnce(t *testing.T) {
	f := newFixture(t)
	b := &blockingFirer{fakeFirer: fakeFirer{result: FireResult{Status: cron.StatusOK}},
		entered: make(chan struct{}, 4), release: make(chan struct{})}
	f.h.Fire = b.fire
	hdr := func() map[string]string {
		h := f.signed([]byte(`{}`))
		h[DeliveryHeader] = "same-id"
		return h
	}
	codes := concurrentPosts(t, f, b, hdr(), hdr())
	fires := 0
	for _, c := range b.calls {
		if c.Skip == "" {
			fires++
		}
	}
	if fires != 1 {
		t.Fatalf("one delivery id fired %d times (codes %v)", fires, codes)
	}
}

// ...and the rate slot too: two concurrent POSTs without ids fire once; the
// other is recorded as a rate-limited skip.
func TestHandlerConcurrentPostsReserveTheRateSlot(t *testing.T) {
	f := newFixture(t)
	b := &blockingFirer{fakeFirer: fakeFirer{result: FireResult{Status: cron.StatusOK}},
		entered: make(chan struct{}, 4), release: make(chan struct{})}
	f.h.Fire = b.fire
	codes := concurrentPosts(t, f, b, f.signed([]byte(`{}`)), f.signed([]byte(`{}`)))
	fires := 0
	for _, c := range b.calls {
		if c.Skip == "" {
			fires++
		}
	}
	if fires != 1 {
		t.Fatalf("concurrent POSTs fired %d times (codes %v)", fires, codes)
	}
}

// A reservation is released when the fire did not happen: a delivery that was
// blocked by policy may be redelivered once the policy is fixed.
func TestHandlerReleasesDeliveryWhenNotFired(t *testing.T) {
	f := newFixture(t)
	f.firer.result = FireResult{Status: cron.StatusBlocked}
	body := []byte(`{}`)
	hdr := f.signed(body)
	hdr[DeliveryHeader] = "retry-me"
	if rec := f.post(t, "/t/gh-push", body, hdr); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked code = %d", rec.Code)
	}
	f.firer.result = FireResult{Status: cron.StatusOK}
	if rec := f.post(t, "/t/gh-push", body, hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("redelivery after a blocked fire = %d, want 202", rec.Code)
	}
}
