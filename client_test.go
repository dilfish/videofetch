package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestClient(referer, ua string) *Client {
	c, err := NewClient("", referer, ua, 2*time.Second)
	if err != nil {
		panic(err)
	}
	return c
}

// echoHeaders replies with selected request headers as plain text.
func echoHeaders() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("User-Agent") + "\n" + r.Header.Get("Referer") + "\n" + r.Header.Get("Content-Type")))
	}
}

func TestNewClientProxySchemes(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:1", "https://127.0.0.1:1", "socks5://127.0.0.1:1", "socks5://user:pw@127.0.0.1:1", "socks5h://127.0.0.1:1", ""} {
		if _, err := NewClient(u, "", "ua", time.Second); err != nil {
			t.Errorf("NewClient(%q): unexpected error %v", u, err)
		}
	}
	for _, u := range []string{"ftp://x", "://bad", "socks4://127.0.0.1:1"} {
		if _, err := NewClient(u, "", "ua", time.Second); err == nil {
			t.Errorf("NewClient(%q): want error, got nil", u)
		}
	}
	if _, err := NewClient("socks5://[::1", "", "ua", time.Second); err == nil {
		t.Error("NewClient(malformed socks url): want error, got nil")
	}
}

func TestClientHeaders(t *testing.T) {
	s := httptest.NewServer(echoHeaders())
	defer s.Close()

	// referer set: UA and Referer must be sent on Get/Head/PostJSON alike
	c := newTestClient("http://ref.example", "test-ua")
	body, err := c.GetBytes(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "test-ua\nhttp://ref.example\n" {
		t.Errorf("Get headers = %q, want UA + referer", got)
	}

	// referer empty: no Referer header; PostJSON sets Content-Type
	c = newTestClient("", "test-ua")
	var out struct {
		X string `json:"x"`
	}
	if err := c.PostJSON(s.URL, []byte(`{"a":1}`), &out); err == nil {
		t.Error("PostJSON: want non-JSON reply error, got nil")
	} else if out.X != "" {
		t.Error("PostJSON: out should stay zero on error")
	}

	// HEAD returns response (status checked by caller)
	resp, err := c.Head(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestPostJSON(t *testing.T) {
	var gotBody string
	var gotCT string
	var gotMethod string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"msg":"hi"}`))
	}))
	defer s.Close()

	c := newTestClient("", "ua")
	var reply struct {
		OK  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	if err := c.PostJSON(s.URL, []byte(`{"password":"pw"}`), &reply); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if !reply.OK || reply.Msg != "hi" {
		t.Errorf("decoded reply = %+v, want ok/msg set", reply)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" || gotBody != `{"password":"pw"}` {
		t.Errorf("request = %s %s body %q", gotMethod, gotCT, gotBody)
	}

	// non-200 but JSON body: still decoded without error (API error shapes)
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"ok":false,"msg":"wrong_password"}`))
	}))
	defer s2.Close()
	if err := c.PostJSON(s2.URL, []byte(`{}`), &reply); err != nil {
		t.Fatalf("PostJSON non-200 JSON: %v", err)
	}
	if reply.Msg != "wrong_password" {
		t.Errorf("reply = %+v, want wrong_password decoded", reply)
	}
}

func TestPostJSONNonJSONReply(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>go away</html>"))
	}))
	defer s.Close()

	c := newTestClient("", "ua")
	var reply map[string]any
	err := c.PostJSON(s.URL, []byte(`{}`), &reply)
	if err == nil {
		t.Fatal("want error for non-JSON reply, got nil")
	}
	if got := err.Error(); got[:4] != "POST" {
		t.Errorf("error = %q, want POST-prefixed", got)
	}
}

func TestGetBytesNon200(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer s.Close()

	c := newTestClient("", "ua")
	_, err := c.GetBytes(s.URL)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("GetBytes: err = %v, want 403 status error", err)
	}
}

func TestGetBytesConnectionError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := s.URL
	s.Close() // nothing listening anymore

	c := newTestClient("", "ua")
	if _, err := c.GetBytes(url); err == nil {
		t.Error("GetBytes on closed server: want error, got nil")
	}
}

// #28: NewClient must force HTTP/2 despite the custom DialContext, which
// silently disables Go's automatic h2. CDNs fingerprint browser-UA-over-
// HTTP/1.1 as a bot pattern — hsex.men (Cloudflare) 403s such requests.
func TestNewClientForceHTTP2(t *testing.T) {
	c := newTestClient("https://example.com/", "ua")
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.http.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true (custom DialContext otherwise pins requests to HTTP/1.1)")
	}
}

// #26: PostJSON decodes a JSON reply into the target and sends the payload
// with Content-Type application/json.
func TestPostJSONSuccess(t *testing.T) {
	var gotBody []byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"answer":42}`)
	}))
	defer s.Close()
	c := newTestClient("", "ua")

	var into struct {
		Answer int `json:"answer"`
	}
	if err := c.PostJSON(s.URL, []byte(`{"q":"meaning"}`), &into); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if into.Answer != 42 {
		t.Errorf("decoded answer = %d, want 42", into.Answer)
	}
	if !strings.Contains(string(gotBody), `"q":"meaning"`) {
		t.Errorf("server saw body %q", gotBody)
	}
}

// #27: PostJSON wraps three failure classes with their cause: a non-JSON
// body (status included in the error), an unreadable body, and a dead
// server (transport error).
func TestPostJSONFailures(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>blocked</html>"))
	}))
	sURL := s.URL
	c := newTestClient("", "ua")

	var into map[string]any
	if err := c.PostJSON(sURL, []byte(`{}`), &into); err == nil || !strings.Contains(err.Error(), "POST "+sURL) {
		t.Errorf("non-JSON body: err = %v, want POST-url error", err)
	}

	s.Close()
	if err := c.PostJSON(sURL, []byte(`{}`), &into); err == nil {
		t.Error("dead server: want error, got nil")
	}
}

// #29: a fresh download whose connection dies mid-body with no prior 206
// fails immediately — the client cannot know the server supports ranges, so
// no retry is attempted.
func TestDownloadDirectNoResumeSupport(t *testing.T) {
	c := newScriptedClient(
		fakeResp(http.StatusOK, 10, errBody("01234")),
		fakeResp(http.StatusOK, 10, io.NopCloser(strings.NewReader("0123456789"))),
	)
	out := filepath.Join(t.TempDir(), "v.mp4")
	err := DownloadDirect(c, "http://x/v.mp4", out)
	if err == nil || !strings.Contains(err.Error(), "does not support resume") {
		t.Fatalf("DownloadDirect: err = %v, want no-resume error", err)
	}
	tr := c.http.Transport.(*scriptedTripper)
	if len(tr.requests) != 1 {
		t.Errorf("attempts = %d, want 1 (cannot resume without a prior 206)", len(tr.requests))
	}
}
