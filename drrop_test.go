package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// #5: drropCode extracts the code from a /v/<code> path and rejects
// everything else (missing segment, empty code, other prefixes).
func TestDrropCode(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v/12345", "12345"},
		{"/v/12345/extra", "12345"},
		{"/v//", ""},
		{"/v/", ""},
		{"/v", ""},
		{"/", ""},
		{"", ""},
		{"/watch/12345", ""},
		{"//v//12345", ""}, // Trim cleans only the outer slashes; the inner // yields an empty segment and is rejected
	}
	for _, tc := range cases {
		if got := drropCode(tc.path); got != tc.want {
			t.Errorf("drropCode(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// #6: isDrropURL accepts drrop.cc / www.drrop.cc video pages (host is
// case-insensitive, scheme optional) and rejects other hosts and pages
// without a /v/<code> path.
func TestIsDrropURL(t *testing.T) {
	cases := []struct {
		u    string
		want bool
	}{
		{"https://drrop.cc/v/12345", true},
		{"https://www.drrop.cc/v/12345", true},
		{"https://DRROP.CC/v/12345", true},
		{"drrop.cc/v/12345", false}, // scheme-less: url.Parse puts everything into Path, Host stays empty
		{"https://evil.example/v/12345", false},
		{"https://sub.drrop.cc/v/12345", false}, // exact host match only
		{"https://drrop.cc/watch/12345", false},
		{"https://drrop.cc/v/", false},
		{"https://drrop.cc/", false},
		{"ht%zz://broken", false},
	}
	for _, tc := range cases {
		if got := isDrropURL(tc.u); got != tc.want {
			t.Errorf("isDrropURL(%q) = %v, want %v", tc.u, got, tc.want)
		}
	}
}

// #7: extractDrrop rejects a page without a /v/<code> path and a missing
// password before any network traffic happens.
func TestExtractDrropValidation(t *testing.T) {
	c, err := NewClient("", "", defaultUA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractDrrop(c, "https://drrop.cc/watch/12345", "pw"); err == nil ||
		!strings.Contains(err.Error(), "not a drrop video page") {
		t.Errorf("no-code page: err = %v, want \"not a drrop video page\"", err)
	}
	if _, err := extractDrrop(c, "https://drrop.cc/v/12345", ""); err == nil ||
		!strings.Contains(err.Error(), "needs a password") {
		t.Errorf("empty password: err = %v, want \"needs a password\"", err)
	}
}

// #24: the extractDrrop network flow — verify-password POST with the JSON
// body {"code","password"}, and the returned token spliced into the signed
// stream URL; every documented error code maps to its message.
func TestExtractDrropFlow(t *testing.T) {
	cases := map[string]struct {
		respond   string
		wantError string
	}{
		"wrong_password": {`{"error":"wrong_password"}`, "drrop: wrong password"},
		"expired":        {`{"error":"expired"}`, "drrop: password attempt expired, retry"},
		"limit_reached":  {`{"error":"limit_reached"}`, "drrop: attempt limit reached"},
		"disabled":       {`{"error":"disabled"}`, "drrop: link disabled"},
		"unknown_code":   {`{"error":"something_new"}`, "drrop: something_new"},
		"no_token":       {`{"expires_in":300}`, "drrop: no token in response"},
	}
	for pw, tc := range cases {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Code     string `json:"code"`
				Password string `json:"password"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Code != "12345" {
				t.Errorf("POST code = %q, want 12345", body.Code)
			}
			if body.Password != pw {
				t.Errorf("POST password = %q, want %q", body.Password, pw)
			}
			fmt.Fprint(w, tc.respond)
		}))

		old := drropAPIBase
		drropAPIBase = s.URL

		c, err := NewClient("", "", defaultUA, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = extractDrrop(c, "https://drrop.cc/v/12345", pw)
		drropAPIBase = old
		s.Close()
		if err == nil || !strings.Contains(err.Error(), tc.wantError) {
			t.Errorf("extractDrrop(pw=%q): err = %v, want %q", pw, err, tc.wantError)
		}
	}
}

// #25: a successful verify-password returns the signed stream URL
// <base>/api/stream/<code>?t=<escaped token> as a direct stream labelled
// drrop.
func TestExtractDrropSuccess(t *testing.T) {
	const token = "stream:1259579.1794725964.deadbeef"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"`+token+`","expires_in":300,"engage_token":"ignore-me"}`)
	}))

	old := drropAPIBase
	drropAPIBase = s.URL
	t.Cleanup(func() { drropAPIBase = old })

	c, err := NewClient("", "", defaultUA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := extractDrrop(c, "https://drrop.cc/v/1259579", "1113")
	if err != nil {
		t.Fatalf("extractDrrop: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("got %d streams, want 1", len(streams))
	}
	want := s.URL + "/api/stream/1259579?t=" + url.QueryEscape(token)
	if streams[0].URL != want {
		t.Errorf("stream URL = %q, want %q", streams[0].URL, want)
	}
	if streams[0].Kind != "direct" || streams[0].Label != "drrop" {
		t.Errorf("Kind/Label = %q/%q, want direct/drrop", streams[0].Kind, streams[0].Label)
	}
}
