// SPDX-License-Identifier: Apache-2.0

package channels

// Fixture tests for the WeCom adapter. All offline: the crypto and parsing are
// pure, and API calls go to an httptest server via apiBase.
//
// The crypto tests carry more weight here than usual. WeCom encrypts every
// callback, so a mistake in wecom_crypt.go does not degrade the channel — it
// makes it completely inert, and the only symptom is a URL that will not
// validate. Since the implementation cannot be checked against a live corp
// without an account, these tests pin it against the DOCUMENTED byte layout
// (developer.work.weixin.qq.com/document/path/90968) rather than against
// whatever the code happens to produce: they assert the 16-byte random prefix,
// the big-endian length field, the receiveid trailer and the 32-byte padding
// independently, so a round-trip that is self-consistently wrong still fails.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// testAESKey is a valid 43-character EncodingAESKey (43 base64 chars → 32 bytes).
const testAESKey = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

const testCorpID = "wwcorp1234567890"

func mustAESKey(t *testing.T) []byte {
	t.Helper()
	key, err := decodeWeComAESKey(testAESKey)
	if err != nil {
		t.Fatalf("decodeWeComAESKey: %v", err)
	}
	return key
}

func TestDecodeWeComAESKey(t *testing.T) {
	key := mustAESKey(t)
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}

	bad := map[string]string{
		"too short":  "abc",
		"too long":   testAESKey + "XX",
		"not base64": strings.Repeat("!", 43),
		"empty":      "",
	}
	for name, k := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeWeComAESKey(k); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The round trip proves encrypt and decrypt agree with each other.
func TestWeComCryptRoundTrip(t *testing.T) {
	key := mustAESKey(t)
	for _, msgText := range []string{
		"hello",
		"你好，这是一条中文消息",             // multibyte
		"",                        // empty message
		strings.Repeat("x", 5000), // spans many blocks
		"<xml><Content><![CDATA[hi]]></Content></xml>",
	} {
		enc, err := weComEncrypt(key, []byte(msgText), testCorpID)
		if err != nil {
			t.Fatalf("encrypt(%.20q): %v", msgText, err)
		}
		got, err := weComDecrypt(key, enc, testCorpID)
		if err != nil {
			t.Fatalf("decrypt(%.20q): %v", msgText, err)
		}
		if string(got) != msgText {
			t.Errorf("round trip mismatch: got %.40q, want %.40q", got, msgText)
		}
	}
}

// Independently verify the plaintext LAYOUT against the spec, so a
// self-consistent but wrong implementation (e.g. little-endian length) fails
// here even though the round trip above would pass.
func TestWeComPlaintextLayoutMatchesSpec(t *testing.T) {
	key := mustAESKey(t)
	const payload = "layout-check"

	enc, err := weComEncrypt(key, []byte(payload), testCorpID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	// Decrypt by hand rather than through weComDecrypt.
	plain := aesCBCDecryptForTest(t, key, raw)
	plain, err = pkcs7Unpad(plain)
	if err != nil {
		t.Fatal(err)
	}

	if len(plain) < 20 {
		t.Fatalf("plaintext too short: %d", len(plain))
	}
	// random(16) ‖ msgLen(4, BIG-endian) ‖ msg ‖ receiveid
	gotLen := binary.BigEndian.Uint32(plain[16:20])
	if int(gotLen) != len(payload) {
		t.Errorf("big-endian length = %d, want %d (is the length field little-endian?)", gotLen, len(payload))
	}
	if got := string(plain[20 : 20+gotLen]); got != payload {
		t.Errorf("message = %q, want %q", got, payload)
	}
	if got := string(plain[20+gotLen:]); got != testCorpID {
		t.Errorf("receiveid trailer = %q, want %q", got, testCorpID)
	}
}

func TestWeComPaddingIsMultipleOf32(t *testing.T) {
	key := mustAESKey(t)
	// A message whose header+body already lands on 32 must still gain a full
	// pad block, or unpadding would be ambiguous.
	for _, n := range []int{0, 1, 11, 12, 44, 100} {
		enc, err := weComEncrypt(key, []byte(strings.Repeat("a", n)), testCorpID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(enc)
		if len(raw)%weComPadBlock != 0 {
			t.Errorf("msg len %d → ciphertext %d bytes, not a multiple of %d", n, len(raw), weComPadBlock)
		}
	}
}

func TestWeComDecryptRejectsBadInput(t *testing.T) {
	key := mustAESKey(t)
	good, err := weComEncrypt(key, []byte("hi"), testCorpID)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("wrong receiveid", func(t *testing.T) {
		// A payload encrypted for another corp must be refused, not processed.
		if _, err := weComDecrypt(key, good, "wwOTHERCORP"); err == nil {
			t.Error("expected rejection for a mismatched corpid")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		other, _ := decodeWeComAESKey("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ")
		if _, err := weComDecrypt(other, good, testCorpID); err == nil {
			t.Error("expected failure decrypting with the wrong key")
		}
	})
	t.Run("not base64", func(t *testing.T) {
		if _, err := weComDecrypt(key, "!!!not base64!!!", testCorpID); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("truncated ciphertext", func(t *testing.T) {
		raw, _ := base64.StdEncoding.DecodeString(good)
		short := base64.StdEncoding.EncodeToString(raw[:len(raw)-5])
		if _, err := weComDecrypt(key, short, testCorpID); err == nil {
			t.Error("expected an error for a non-block-aligned ciphertext")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := weComDecrypt(key, "", testCorpID); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestPKCS7Unpad(t *testing.T) {
	bad := map[string][]byte{
		"empty":               {},
		"zero pad byte":       {1, 2, 0},
		"pad longer than buf": {1, 9},
		"pad over 32":         {1, 2, 33},
		"inconsistent":        {1, 2, 3, 3, 2},
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := pkcs7Unpad(b); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestWeComSignatureValid(t *testing.T) {
	const token, ts, nonce, enc = "wc-token", "1735689600", "n123", "CIPHERTEXT=="
	good := weComSignature(token, ts, nonce, enc)

	if !weComSignatureValid(token, ts, nonce, enc, good) {
		t.Error("a correct signature should validate")
	}
	if !weComSignatureValid(token, ts, nonce, enc, strings.ToUpper(good)) {
		t.Error("signature comparison should be case-insensitive")
	}
	// The signature covers the ciphertext — tampering with the body must break it.
	if weComSignatureValid(token, ts, nonce, "TAMPERED==", good) {
		t.Error("a modified body must invalidate the signature")
	}
	if weComSignatureValid("other", ts, nonce, enc, good) {
		t.Error("a different token must not validate")
	}
	if weComSignatureValid("", ts, nonce, enc, weComSignature("", ts, nonce, enc)) {
		t.Error("an empty token must never validate")
	}
}

// The verification handshake is where WeCom differs most sharply from WeChat:
// echoing echostr back verbatim (the WeChat behaviour) fails here.
func TestWeComVerifyReturnsDecryptedPlaintext(t *testing.T) {
	key := mustAESKey(t)
	w := NewWeComAdapter(msg.ChannelConfig{
		AppID: testCorpID, VerifyToken: "wc-token", EncodingAESKey: testAESKey,
	})
	w.aesKey = key

	const plaintext = "1234567890123456"
	echo, err := weComEncrypt(key, []byte(plaintext), testCorpID)
	if err != nil {
		t.Fatal(err)
	}
	sig := weComSignature("wc-token", "1735689600", "n1", echo)

	req := httptest.NewRequest(http.MethodGet,
		"/?msg_signature="+sig+"&timestamp=1735689600&nonce=n1&echostr="+urlQueryEscape(echo), nil)
	rw := httptest.NewRecorder()
	w.handleWebhook(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	body := rw.Body.String()
	if body != plaintext {
		t.Errorf("body = %q, want the DECRYPTED plaintext %q", body, plaintext)
	}
	if body == echo {
		t.Error("echoed the ciphertext verbatim — that is the WeChat behaviour and fails WeCom validation")
	}
}

func TestWeComVerifyRejectsBadSignature(t *testing.T) {
	w := NewWeComAdapter(msg.ChannelConfig{
		AppID: testCorpID, VerifyToken: "wc-token", EncodingAESKey: testAESKey,
	})
	w.aesKey = mustAESKey(t)

	req := httptest.NewRequest(http.MethodGet,
		"/?msg_signature=deadbeef&timestamp=1&nonce=n&echostr=whatever", nil)
	rw := httptest.NewRecorder()
	w.handleWebhook(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rw.Code)
	}
}

func TestWeComInboundDecryptsAndForwards(t *testing.T) {
	key := mustAESKey(t)
	w := NewWeComAdapter(msg.ChannelConfig{
		AppID: testCorpID, VerifyToken: "wc-token", EncodingAESKey: testAESKey,
	})
	w.aesKey = key

	inner := `<xml><ToUserName><![CDATA[` + testCorpID + `]]></ToUserName>` +
		`<FromUserName><![CDATA[zhangsan]]></FromUserName>` +
		`<CreateTime>1735689600</CreateTime>` +
		`<MsgType><![CDATA[text]]></MsgType>` +
		`<Content><![CDATA[你好 rysh]]></Content>` +
		`<MsgId>1234567890123456</MsgId><AgentID>1000002</AgentID></xml>`

	enc, err := weComEncrypt(key, []byte(inner), testCorpID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := xml.Marshal(weComEnvelope{ToUserName: testCorpID, Encrypt: enc})
	if err != nil {
		t.Fatal(err)
	}
	sig := weComSignature("wc-token", "1735689600", "n1", enc)

	req := httptest.NewRequest(http.MethodPost,
		"/?msg_signature="+sig+"&timestamp=1735689600&nonce=n1", strings.NewReader(string(envelope)))
	rw := httptest.NewRecorder()
	w.handleWebhook(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	select {
	case im := <-w.InboundCh():
		if im.SenderID != "zhangsan" {
			t.Errorf("SenderID = %q", im.SenderID)
		}
		if im.Content != "你好 rysh" {
			t.Errorf("Content = %q", im.Content)
		}
		if im.ThreadID != "zhangsan" {
			t.Errorf("ThreadID = %q, want the sender", im.ThreadID)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound message delivered")
	}
}

func TestWeComInboundRejectsForgedSignature(t *testing.T) {
	key := mustAESKey(t)
	w := NewWeComAdapter(msg.ChannelConfig{
		AppID: testCorpID, VerifyToken: "wc-token", EncodingAESKey: testAESKey,
	})
	w.aesKey = key

	enc, _ := weComEncrypt(key, []byte(`<xml><MsgType><![CDATA[text]]></MsgType>`+
		`<Content><![CDATA[injected]]></Content></xml>`), testCorpID)
	envelope, _ := xml.Marshal(weComEnvelope{ToUserName: testCorpID, Encrypt: enc})

	req := httptest.NewRequest(http.MethodPost,
		"/?msg_signature=00000000&timestamp=1&nonce=n", strings.NewReader(string(envelope)))
	rw := httptest.NewRecorder()
	w.handleWebhook(rw, req)

	if rw.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rw.Code)
	}
	select {
	case im := <-w.InboundCh():
		t.Fatalf("an unsigned payload reached the humanoid: %q", im.Content)
	default:
	}
}

func TestParseWeComInboundIgnoresNonText(t *testing.T) {
	cases := map[string]string{
		"event":   `<xml><FromUserName><![CDATA[u]]></FromUserName><MsgType><![CDATA[event]]></MsgType></xml>`,
		"image":   `<xml><FromUserName><![CDATA[u]]></FromUserName><MsgType><![CDATA[image]]></MsgType></xml>`,
		"blank":   `<xml><FromUserName><![CDATA[u]]></FromUserName><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[ ]]></Content></xml>`,
		"garbage": `nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseWeComInbound([]byte(body)); len(got) != 0 {
				t.Errorf("expected no messages, got %d", len(got))
			}
		})
	}
}

func newWeComTestAdapter(t *testing.T, h http.HandlerFunc) *WeComAdapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	w := NewWeComAdapter(msg.ChannelConfig{
		AppID: testCorpID, AppSecret: "secret", AgentID: "1000002",
		VerifyToken: "wc-token", EncodingAESKey: testAESKey,
	})
	w.apiBase = srv.URL
	return w
}

func TestWeComAccessTokenCaches(t *testing.T) {
	var fetches int
	w := newWeComTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "tok", "expires_in": 7200})
	})
	for i := 0; i < 3; i++ {
		if _, err := w.accessToken(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fetches != 1 {
		t.Errorf("fetched %d times, want 1", fetches)
	}
}

func TestWeComSendIncludesAgentID(t *testing.T) {
	var got map[string]any
	w := newWeComTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 7200})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(rw).Encode(map[string]any{"errcode": 0, "errmsg": "ok"})
	})

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "zhangsan", Content: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// agentid is mandatory and must be a NUMBER, not the config's string.
	if _, ok := got["agentid"].(float64); !ok {
		t.Errorf("agentid = %#v, want a number", got["agentid"])
	}
	if got["touser"] != "zhangsan" {
		t.Errorf("touser = %v", got["touser"])
	}
}

func TestWeComSendSplitsLongContent(t *testing.T) {
	var sends int
	w := newWeComTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 7200})
			return
		}
		sends++
		_ = json.NewEncoder(rw).Encode(map[string]any{"errcode": 0})
	})
	// WeCom TRUNCATES past 2048 bytes rather than erroring, so splitting is
	// what stops a clipped answer from looking like a complete one.
	long := strings.Repeat("a", weComMaxBodyLen*2+50)
	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "u", Content: long}); err != nil {
		t.Fatal(err)
	}
	if sends < 3 {
		t.Errorf("sends = %d, want the content split into at least 3", sends)
	}
}

func TestWeComSendErrorHint(t *testing.T) {
	w := newWeComTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 7200})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"errcode": weComErrNotAllowedUser, "errmsg": "not allowed"})
	})
	err := w.Send(context.Background(), OutboundMessage{RecipientID: "u", Content: "hi"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "visible range") {
		t.Errorf("error should carry the operator hint, got %q", err)
	}
}

func TestWeComStartRequiresCredentials(t *testing.T) {
	base := msg.ChannelConfig{
		AppID: testCorpID, AppSecret: "s", AgentID: "1", VerifyToken: "t", EncodingAESKey: testAESKey,
	}
	tests := []struct {
		name   string
		mutate func(c *msg.ChannelConfig)
		want   string
	}{
		{"no corpid", func(c *msg.ChannelConfig) { c.AppID = "" }, "app_id is required"},
		{"no secret", func(c *msg.ChannelConfig) { c.AppSecret = "" }, "app_secret is required"},
		{"no agent_id", func(c *msg.ChannelConfig) { c.AgentID = "" }, "agent_id is required"},
		{"no token", func(c *msg.ChannelConfig) { c.VerifyToken = "" }, "token is required"},
		// The WeChat-vs-WeCom trap: an absent key is fatal here, not optional.
		{"no aes key", func(c *msg.ChannelConfig) { c.EncodingAESKey = "" }, "encoding_aes_key is required"},
		{"bad aes key", func(c *msg.ChannelConfig) { c.EncodingAESKey = "short" }, "encoding_aes_key is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			w := NewWeComAdapter(cfg)
			err := w.Start(context.Background())
			if err == nil {
				_ = w.Stop()
				t.Fatal("expected Start to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWeComFactoryAndValidType(t *testing.T) {
	if !IsValidChannelType("wecom") {
		t.Error("wecom should be a valid channel type")
	}
	a, err := NewAdapter("wecom", msg.ChannelConfig{AppID: testCorpID})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if a.Type() != "wecom" {
		t.Errorf("Type() = %q", a.Type())
	}
}

func TestWeComMarkHandledDedupes(t *testing.T) {
	w := NewWeComAdapter(msg.ChannelConfig{})
	if !w.markHandled("m1") {
		t.Error("first delivery should be new")
	}
	if w.markHandled("m1") {
		t.Error("a retry of the same MsgId should be a duplicate")
	}
	for i := 0; i < weComHandledCap+5; i++ {
		w.markHandled(fmt.Sprintf("x%d", i))
	}
	if len(w.handled) > weComHandledCap {
		t.Errorf("handled set = %d, cap %d", len(w.handled), weComHandledCap)
	}
}

// aesCBCDecryptForTest decrypts without going through weComDecrypt, so the
// layout test can inspect the raw plaintext independently of the code it is
// checking.
func aesCBCDecryptForTest(t *testing.T, key, raw []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(out, raw)
	return out
}

// urlQueryEscape is url.QueryEscape, wrapped so the import reads at the call site.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }
