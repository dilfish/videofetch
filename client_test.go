package main

import (
	"net/http"
	"net/http/httptest"
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
