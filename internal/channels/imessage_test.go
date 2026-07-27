package channels

// Unit tests for the iMessage adapter (C5, design 001 §4.5). Everything here
// runs on Linux: the macOS host bridge (sqlite3 CLI, osascript) is stubbed
// through the adapter's unexported function-typed seams.

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// --- typedstream fixture helpers -------------------------------------------

// buildTypedstreamFixture constructs a synthetic attributedBody blob:
// leading junk, the "NSString" class marker, realistic intermediate bytes
// ending in the 0x01 0x2b marker, a length encoding, then the payload.
func buildTypedstreamFixture(payload []byte, wideLength bool) []byte {
	blob := []byte("\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84\x12NSAttributedString\x00\x84\x84\x08NSObject\x00\x85\x92\x84\x84\x84")
	blob = append(blob, []byte("NSString")...)
	// Realistic post-class bytes: 0x01 0x94 0x84 0x01 0x2b — the extractor
	// scans for the trailing 0x01 0x2b pair.
	blob = append(blob, 0x01, 0x94, 0x84, 0x01, 0x2b)
	if wideLength {
		blob = append(blob, 0x81, byte(len(payload)&0xff), byte(len(payload)>>8))
	} else {
		blob = append(blob, byte(len(payload)))
	}
	blob = append(blob, payload...)
	// Trailing typedstream noise.
	blob = append(blob, 0x86, 0x84, 0x02, 0x69, 0x49, 0x01)
	return blob
}

func TestExtractAttributedBodyText(t *testing.T) {
	longText := strings.Repeat("rysh iMessage bridge! ", 15) // 330 bytes > 0xff
	if len(longText) <= 0xff {
		t.Fatalf("fixture bug: long text must exceed 255 bytes, got %d", len(longText))
	}

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "simple ascii, single-byte length",
			raw:  buildTypedstreamFixture([]byte("Hello from iMessage"), false),
			want: "Hello from iMessage",
		},
		{
			name: "utf-8 payload, single-byte length",
			raw:  buildTypedstreamFixture([]byte("héllo wörld ✓"), false),
			want: "héllo wörld ✓",
		},
		{
			name: "long string with 0x81 little-endian uint16 length",
			raw:  buildTypedstreamFixture([]byte(longText), true),
			want: longText,
		},
		{
			name: "garbage blob without NSString",
			raw:  []byte{0x04, 0x0b, 0xde, 0xad, 0xbe, 0xef, 0x01, 0x2b, 0x05, 'h', 'e', 'l', 'l', 'o'},
			want: "",
		},
		{
			name: "NSString present but no 0x01 0x2b marker",
			raw:  append([]byte("junkNSString"), 0x00, 0x00, 0x00, 0x00),
			want: "",
		},
		{
			name: "truncated: length runs past end of blob",
			raw:  append(append([]byte("NSString"), 0x01, 0x2b), 0x50, 'h', 'i'),
			want: "",
		},
		{
			name: "truncated: 0x81 without its two length bytes",
			raw:  append(append([]byte("NSString"), 0x01, 0x2b), 0x81),
			want: "",
		},
		{
			name: "unknown wide-length marker 0x82 gives up",
			raw:  append(append([]byte("NSString"), 0x01, 0x2b), 0x82, 0x01, 0x00, 0x00, 0x00, 'x'),
			want: "",
		},
		{
			name: "zero length rejected",
			raw:  append(append([]byte("NSString"), 0x01, 0x2b), 0x00),
			want: "",
		},
		{
			name: "invalid utf-8 payload rejected",
			raw:  append(append([]byte("NSString"), 0x01, 0x2b), 0x03, 0xff, 0xfe, 0xfd),
			want: "",
		},
		{
			name: "empty blob",
			raw:  nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractAttributedBodyText(tt.raw); got != tt.want {
				t.Errorf("extractAttributedBodyText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- sqlite3 -json row parsing → InboundMessage ----------------------------

func strPtr(s string) *string { return &s }

func TestParseIMessageRows(t *testing.T) {
	t.Run("empty output means no rows", func(t *testing.T) {
		for _, out := range [][]byte{nil, []byte(""), []byte("\n")} {
			rows, err := parseIMessageRows(out)
			if err != nil || len(rows) != 0 {
				t.Errorf("parseIMessageRows(%q) = %v, %v; want empty, nil", out, rows, err)
			}
		}
	})
	t.Run("malformed json errors", func(t *testing.T) {
		if _, err := parseIMessageRows([]byte(`{"not":"an array"`)); err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
	t.Run("null text and null chat_guid decode as nil pointers", func(t *testing.T) {
		out := []byte(`[{"rowid":7,"text":null,"is_from_me":0,"date":712345678901234,"handle":"+15551234567","chat_guid":null,"service":"iMessage","attributed_body_hex":null}]`)
		rows, err := parseIMessageRows(out)
		if err != nil {
			t.Fatalf("parseIMessageRows: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		r := rows[0]
		if r.RowID != 7 || r.Text != nil || r.ChatGUID != nil || r.AttributedBodyHex != nil {
			t.Errorf("unexpected row: %+v", r)
		}
		if r.Handle != "+15551234567" || r.Service == nil || *r.Service != "iMessage" {
			t.Errorf("handle/service mismatch: %+v", r)
		}
	})
}

func TestParseIMessageMaxRowID(t *testing.T) {
	tests := []struct {
		name    string
		out     []byte
		want    int64
		wantErr bool
	}{
		{"normal", []byte(`[{"max_rowid":4211}]` + "\n"), 4211, false},
		{"empty table via COALESCE", []byte(`[{"max_rowid":0}]`), 0, false},
		{"no output at all", nil, 0, false},
		{"garbage", []byte("not json"), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIMessageMaxRowID(tt.out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIMessageRowToInbound(t *testing.T) {
	attributedHex := strings.ToUpper(hex.EncodeToString(
		buildTypedstreamFixture([]byte("decoded from typedstream"), false)))
	garbageHex := "DEADBEEF00112233"

	allowedSet := map[string]bool{"+15559876543": true}

	tests := []struct {
		name        string
		row         imessageRow
		allowed     map[string]bool
		wantOK      bool
		wantContent string
		wantThread  string
		wantMeta    map[string]string
	}{
		{
			name: "plain text row, allow-all, chat_guid threading",
			row: imessageRow{
				RowID: 42, Text: strPtr("hi rysh"), Handle: "+15559876543",
				ChatGUID: strPtr("iMessage;-;+15559876543"), Service: strPtr("iMessage"),
			},
			allowed:     map[string]bool{},
			wantOK:      true,
			wantContent: "hi rysh",
			wantThread:  "iMessage;-;+15559876543",
			wantMeta: map[string]string{
				"rowid": "42", "chat_guid": "iMessage;-;+15559876543", "service": "iMessage",
			},
		},
		{
			name: "allowed handle passes filter",
			row: imessageRow{
				RowID: 43, Text: strPtr("allowed"), Handle: "+15559876543",
				ChatGUID: strPtr("iMessage;-;+15559876543"), Service: strPtr("iMessage"),
			},
			allowed:     allowedSet,
			wantOK:      true,
			wantContent: "allowed",
			wantThread:  "iMessage;-;+15559876543",
		},
		{
			name: "disallowed handle filtered out",
			row: imessageRow{
				RowID: 44, Text: strPtr("spam"), Handle: "+15550000000",
				ChatGUID: strPtr("iMessage;-;+15550000000"), Service: strPtr("iMessage"),
			},
			allowed: allowedSet,
			wantOK:  false,
		},
		{
			name: "NULL text with decodable attributedBody",
			row: imessageRow{
				RowID: 45, Text: nil, Handle: "user@example.com",
				ChatGUID: strPtr("iMessage;-;user@example.com"), Service: strPtr("iMessage"),
				AttributedBodyHex: strPtr(attributedHex),
			},
			allowed:     map[string]bool{},
			wantOK:      true,
			wantContent: "decoded from typedstream",
			wantThread:  "iMessage;-;user@example.com",
		},
		{
			name: "NULL text with undecodable attributedBody falls back honestly",
			row: imessageRow{
				RowID: 46, Text: nil, Handle: "+15559876543",
				ChatGUID: strPtr("chat123456"), Service: strPtr("iMessage"),
				AttributedBodyHex: strPtr(garbageHex),
			},
			allowed:     map[string]bool{},
			wantOK:      true,
			wantContent: imessageUnsupportedBody,
			wantThread:  "chat123456",
		},
		{
			name: "NULL chat_guid falls back to handle as thread",
			row: imessageRow{
				RowID: 47, Text: strPtr("sms text"), Handle: "+15551112222",
				ChatGUID: nil, Service: strPtr("SMS"),
			},
			allowed:     map[string]bool{},
			wantOK:      true,
			wantContent: "sms text",
			wantThread:  "+15551112222",
			wantMeta: map[string]string{
				"rowid": "47", "chat_guid": "", "service": "SMS",
			},
		},
		{
			name: "no text and no attributedBody skipped (attachment/tapback)",
			row: imessageRow{
				RowID: 48, Text: nil, Handle: "+15559876543",
				ChatGUID: strPtr("g"), Service: strPtr("iMessage"),
			},
			allowed: map[string]bool{},
			wantOK:  false,
		},
		{
			name: "is_from_me row skipped defensively",
			row: imessageRow{
				RowID: 49, Text: strPtr("me"), IsFromMe: 1, Handle: "+15559876543",
			},
			allowed: map[string]bool{},
			wantOK:  false,
		},
		{
			name:    "empty handle skipped",
			row:     imessageRow{RowID: 50, Text: strPtr("x"), Handle: ""},
			allowed: map[string]bool{},
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, ok := imessageRowToInbound(tt.row, tt.allowed)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (in=%+v)", ok, tt.wantOK, in)
			}
			if !ok {
				return
			}
			if in.SenderID != tt.row.Handle || in.SenderName != tt.row.Handle {
				t.Errorf("SenderID/SenderName = %q/%q, want handle %q (honest fallback)",
					in.SenderID, in.SenderName, tt.row.Handle)
			}
			if in.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", in.Content, tt.wantContent)
			}
			if in.ThreadID != tt.wantThread {
				t.Errorf("ThreadID = %q, want %q", in.ThreadID, tt.wantThread)
			}
			for k, v := range tt.wantMeta {
				if in.Metadata[k] != v {
					t.Errorf("Metadata[%q] = %q, want %q", k, in.Metadata[k], v)
				}
			}
		})
	}
}

// --- buddy vs chat-GUID detection + osascript argv construction ------------

func TestIsIMessageChatGUID(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"+15551234567", false},
		{"user@example.com", false},
		{"iMessage;-;+15551234567", true},
		{"iMessage;+;chat447221234567890", true},
		{"SMS;-;+15551234567", true},
		{"anything;with;semicolons", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := isIMessageChatGUID(tt.target); got != tt.want {
			t.Errorf("isIMessageChatGUID(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

func TestBuildIMessageOSASend(t *testing.T) {
	// Content laced with AppleScript metacharacters must land in argv, never
	// in the script source.
	evil := `" & (do shell script "rm -rf ~") & "`

	t.Run("buddy handle", func(t *testing.T) {
		script, args := buildIMessageOSASend(evil, "+15551234567")
		if !strings.Contains(script, "on run argv") {
			t.Error("script must receive content via `on run argv`")
		}
		if !strings.Contains(script, "buddy theBuddy") {
			t.Errorf("buddy target must use the buddy script, got: %s", script)
		}
		if strings.Contains(script, evil) {
			t.Error("user content leaked into AppleScript source (injection)")
		}
		if len(args) != 2 || args[0] != evil || args[1] != "+15551234567" {
			t.Errorf("args = %q, want [content, target]", args)
		}
	})

	t.Run("chat guid", func(t *testing.T) {
		script, args := buildIMessageOSASend("hello", "iMessage;+;chat447221234")
		if !strings.Contains(script, "chat id theChatID") {
			t.Errorf("chat GUID target must use the chat script, got: %s", script)
		}
		if !strings.Contains(script, "on run argv") {
			t.Error("script must receive content via `on run argv`")
		}
		if len(args) != 2 || args[0] != "hello" || args[1] != "iMessage;+;chat447221234" {
			t.Errorf("args = %q, want [content, target]", args)
		}
	})
}

// --- Send behavior ----------------------------------------------------------

func TestIMessageSendStepSuppressed(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{})
	var calls int32
	a.runOSA = func(ctx context.Context, script string, args []string) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	err := a.Send(context.Background(), OutboundMessage{
		RecipientID: "+15551234567",
		Content:     "Running tests...",
		Kind:        OutboundKindStep,
	})
	if err != nil {
		t.Fatalf("step send must be suppressed with nil error, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("osascript invoked %d times for a step message, want 0 (§4.7 suppression)", n)
	}
}

func TestIMessageSend(t *testing.T) {
	t.Run("normal send to buddy via RecipientID", func(t *testing.T) {
		a := NewIMessageAdapter(msg.ChannelConfig{})
		var gotScript string
		var gotArgs []string
		a.runOSA = func(ctx context.Context, script string, args []string) error {
			gotScript, gotArgs = script, args
			return nil
		}
		err := a.Send(context.Background(), OutboundMessage{
			RecipientID: "+15551234567", Content: "hi there",
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !strings.Contains(gotScript, "buddy theBuddy") {
			t.Errorf("expected buddy script, got: %s", gotScript)
		}
		if len(gotArgs) != 2 || gotArgs[0] != "hi there" || gotArgs[1] != "+15551234567" {
			t.Errorf("args = %q", gotArgs)
		}
	})

	t.Run("falls back to ThreadID chat GUID when RecipientID empty", func(t *testing.T) {
		a := NewIMessageAdapter(msg.ChannelConfig{})
		var gotScript string
		var gotArgs []string
		a.runOSA = func(ctx context.Context, script string, args []string) error {
			gotScript, gotArgs = script, args
			return nil
		}
		err := a.Send(context.Background(), OutboundMessage{
			ThreadID: "iMessage;-;+15559876543", Content: "threaded",
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !strings.Contains(gotScript, "chat id theChatID") {
			t.Errorf("expected chat script for GUID target, got: %s", gotScript)
		}
		if len(gotArgs) != 2 || gotArgs[1] != "iMessage;-;+15559876543" {
			t.Errorf("args = %q", gotArgs)
		}
	})

	t.Run("no recipient errors", func(t *testing.T) {
		a := NewIMessageAdapter(msg.ChannelConfig{})
		a.runOSA = func(ctx context.Context, script string, args []string) error { return nil }
		if err := a.Send(context.Background(), OutboundMessage{Content: "orphan"}); err == nil {
			t.Error("expected error when RecipientID and ThreadID are both empty")
		}
	})

	t.Run("osascript failure surfaces", func(t *testing.T) {
		a := NewIMessageAdapter(msg.ChannelConfig{})
		a.runOSA = func(ctx context.Context, script string, args []string) error {
			return fmt.Errorf("osascript: not authorized")
		}
		err := a.Send(context.Background(), OutboundMessage{RecipientID: "+15551", Content: "x"})
		if err == nil || !strings.Contains(err.Error(), "AppleScript send failed") {
			t.Errorf("expected wrapped AppleScript error, got %v", err)
		}
		if st := a.Status(); st.Error == "" {
			t.Error("send failure should surface in Status().Error")
		}
	})
}

// --- Start gating -----------------------------------------------------------

func TestIMessageStartNonDarwin(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{})
	a.goos = "linux" // explicit: this test must behave identically on any CI OS
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("Start must fail on non-darwin hosts")
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("error should name the macOS requirement, got: %v", err)
	}
}

func TestIMessageStartMissingSqlite3(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{DBPath: "/tmp/fake-chat.db"})
	a.goos = "darwin"
	a.statPath = func(string) error { return nil }
	a.lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	err := a.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sqlite3") {
		t.Errorf("expected sqlite3-not-found error, got %v", err)
	}
}

func TestIMessageStartPermissionHint(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{DBPath: "/tmp/fake-chat.db"})
	a.goos = "darwin"
	a.statPath = func(string) error { return nil }
	a.lookPath = func(string) (string, error) { return "/usr/bin/sqlite3", nil }
	a.runSQL = func(ctx context.Context, dbPath, query string) ([]byte, error) {
		return nil, fmt.Errorf("sqlite3: exit status 1: Error: unable to open database file")
	}
	err := a.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Full Disk Access") {
		t.Errorf("permission-shaped failure must name Full Disk Access, got: %v", err)
	}
}

// --- Poll loop with stubbed runSQL ------------------------------------------

func TestIMessagePollLoop(t *testing.T) {
	cfg := msg.ChannelConfig{
		DBPath:   "/tmp/fake-chat.db",
		Channels: []string{"+15559876543"}, // allow-list: one handle
	}
	a := NewIMessageAdapter(cfg)
	a.goos = "darwin"
	a.pollInterval = 5 * time.Millisecond
	a.statPath = func(string) error { return nil }
	a.lookPath = func(string) (string, error) { return "/usr/bin/sqlite3", nil }

	var mu sync.Mutex
	var pollQueries []string
	batchServed := false
	a.runSQL = func(ctx context.Context, dbPath, query string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if dbPath != "/tmp/fake-chat.db" {
			t.Errorf("runSQL got dbPath %q", dbPath)
		}
		if strings.Contains(query, "MAX(ROWID)") {
			return []byte(`[{"max_rowid":100}]`), nil
		}
		pollQueries = append(pollQueries, query)
		if !batchServed {
			batchServed = true
			// Three rows: allowed handle, disallowed handle, allowed again.
			return []byte(`[
				{"rowid":101,"text":"first","is_from_me":0,"date":1,"handle":"+15559876543","chat_guid":"iMessage;-;+15559876543","service":"iMessage","attributed_body_hex":null},
				{"rowid":102,"text":"intruder","is_from_me":0,"date":2,"handle":"+15550001111","chat_guid":"iMessage;-;+15550001111","service":"iMessage","attributed_body_hex":null},
				{"rowid":103,"text":"second","is_from_me":0,"date":3,"handle":"+15559876543","chat_guid":"iMessage;-;+15559876543","service":"iMessage","attributed_body_hex":null}
			]`), nil
		}
		return nil, nil // subsequent ticks: no new rows
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	// lastRowID must be initialized from MAX(ROWID) so only NEW rows flow.
	a.mu.RLock()
	startRowID := a.lastRowID
	a.mu.RUnlock()
	if startRowID != 100 {
		t.Fatalf("lastRowID after Start = %d, want 100", startRowID)
	}

	recv := func() InboundMessage {
		t.Helper()
		select {
		case in := <-a.InboundCh():
			return in
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for inbound message")
			return InboundMessage{}
		}
	}

	first := recv()
	if first.Content != "first" || first.SenderID != "+15559876543" {
		t.Errorf("first inbound = %+v", first)
	}
	if first.ThreadID != "iMessage;-;+15559876543" {
		t.Errorf("first ThreadID = %q", first.ThreadID)
	}
	if first.Metadata["rowid"] != "101" || first.Metadata["service"] != "iMessage" {
		t.Errorf("first metadata = %v", first.Metadata)
	}

	second := recv()
	if second.Content != "second" || second.Metadata["rowid"] != "103" {
		t.Errorf("second inbound = %+v (disallowed handle 102 must be filtered)", second)
	}

	// No third message: the disallowed handle's row was filtered.
	select {
	case extra := <-a.InboundCh():
		t.Errorf("unexpected extra inbound: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}

	// lastRowID advanced past the whole batch, including the filtered row.
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.RLock()
		last := a.lastRowID
		connected := a.connected
		a.mu.RUnlock()
		if last == 103 && connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lastRowID/connected = %d/%v, want 103/true", last, connected)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Poll queries must be parameterized by the tracked lastRowID.
	mu.Lock()
	if len(pollQueries) == 0 {
		mu.Unlock()
		t.Fatal("no poll queries recorded")
	}
	firstQ := pollQueries[0]
	lastQ := pollQueries[len(pollQueries)-1]
	mu.Unlock()
	if !strings.Contains(firstQ, "m.ROWID > 100") {
		t.Errorf("first poll query should start after ROWID 100: %s", firstQ)
	}
	if !strings.Contains(lastQ, "m.ROWID > 103") {
		t.Errorf("later poll queries should advance to ROWID 103: %s", lastQ)
	}
	if !strings.Contains(firstQ, "is_from_me = 0") {
		t.Errorf("poll query must filter own messages: %s", firstQ)
	}

	// Status reflects the connected poll and the allow-list size.
	st := a.Status()
	if !st.Connected {
		t.Error("Status().Connected must be true after a successful poll")
	}
	if st.Type != "imessage" || !strings.Contains(st.Details, "1 handles allowed") {
		t.Errorf("Status = %+v", st)
	}
}

func TestIMessagePollErrorSurfaces(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{DBPath: "/tmp/fake-chat.db"})
	a.goos = "darwin"
	a.pollInterval = 5 * time.Millisecond
	a.statPath = func(string) error { return nil }
	a.lookPath = func(string) (string, error) { return "/usr/bin/sqlite3", nil }
	var polled int32
	a.runSQL = func(ctx context.Context, dbPath, query string) ([]byte, error) {
		if strings.Contains(query, "MAX(ROWID)") {
			return []byte(`[{"max_rowid":5}]`), nil
		}
		atomic.AddInt32(&polled, 1)
		return nil, fmt.Errorf("database is locked")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		st := a.Status()
		if atomic.LoadInt32(&polled) > 0 && st.Error != "" && !st.Connected {
			if !strings.Contains(st.Error, "database is locked") {
				t.Errorf("Status().Error = %q", st.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poll error never surfaced: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- misc -------------------------------------------------------------------

func TestIMessageSetReplyModeNoOp(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{ReplyMode: "messages"})
	a.SetReplyMode("mentions") // stored but behaviorally a no-op (DM-style)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.replyMode != "mentions" {
		t.Errorf("replyMode = %q, want stored value", a.replyMode)
	}
}

func TestIMessageStatusAllHandles(t *testing.T) {
	a := NewIMessageAdapter(msg.ChannelConfig{}) // empty Channels = allow all
	st := a.Status()
	if !strings.Contains(st.Details, "all handles allowed") {
		t.Errorf("Details = %q", st.Details)
	}
	if st.Connected {
		t.Error("must not report connected before first successful poll")
	}
}

func TestBuildIMessagePollQuery(t *testing.T) {
	q := buildIMessagePollQuery(4211)
	for _, want := range []string{
		"m.ROWID > 4211",
		"is_from_me = 0",
		"ORDER BY m.ROWID",
		"h.id AS handle",
		"c.guid AS chat_guid",
		"hex(m.attributedBody)",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %s", want, q)
		}
	}
}
