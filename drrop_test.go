package main

import (
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
