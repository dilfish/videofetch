package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #1: isImageURL accepts image extensions (case-insensitive), query strings
// and protocol-relative URLs, and rejects video/data/non-extension URLs.
func TestIsImageURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://img.example.com/i/2026/a.jpg", true},
		{"https://img.example.com/i/2026/a.JPEG", true},
		{"https://img.example.com/i/2026/a.webp?size=full", true},
		{"https://img.example.com/i/2026/a.png#section", true},
		{"//img.example.com/i/2026/a.gif", true},
		{"  https://img.example.com/i/2026/a.bmp  ", true},
		{"https://img.example.com/video.mp4", false},
		{"https://img.example.com/live.m3u8", false},
		{"https://img.example.com/page", false},
		{"", false},
		{"data:image/png;base64,AAAA", false},
	}
	for _, tc := range cases {
		if got := isImageURL(tc.raw); got != tc.want {
			t.Errorf("isImageURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// #2: urlExt returns the lowercased path extension without the host, query
// and fragment; "" when the path carries no extension or the URL is broken.
func TestUrlExt(t *testing.T) {
	cases := []struct {
		u    string
		want string
	}{
		{"https://h.example.com/dir/name.jpg", ".jpg"},
		{"https://h.example.com/dir/name.MOV", ".mov"},
		{"https://h.example.com/dir/name.webp?x=1#f", ".webp"},
		{"https://h.example.com/dir/noext", ""},
		{"https://h.example.com/", ""},
		{"ht%zz://bad", ""},
	}
	for _, tc := range cases {
		if got := urlExt(tc.u); got != tc.want {
			t.Errorf("urlExt(%q) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

// #3: extractImages walks <img> attributes in imgAttrs priority (lazy-load
// attributes beat src), skips data: URIs, same-host chrome and duplicates,
// and keeps document order with Kind=image / Label=img.
//
// The page lives on server s while the attachments point at a second host
// (att): same-host images — logos, avatars, ad bait — must be skipped, so the
// real attachments have to be off-host for the filter to keep them, exactly
// like a forum page whose images live on a CDN.
func TestExtractImages(t *testing.T) {
	att := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("attachment"))
	}))
	t.Cleanup(att.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
<img src="/logo.png">
<img src="%s/ad-bait.png" data-src="%s/real-attachment-1.jpg">
<img data-src="data:image/gif;base64,AAAA" src="%s/placeholder.png">
<img ess-data="%s/real-attachment-2.webp">
<img data-src="%s/dup-1.jpg"><img src="%s/dup-1.jpg">
<img src="https://other.example.net/pic.jpg">
<img src="/video.mp4">
<img>
</body></html>`, att.URL, att.URL, att.URL, att.URL, att.URL, att.URL)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	c, err := NewClient("", "", defaultUA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := c.extractImages(s.URL + "/page.html")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		att.URL + "/real-attachment-1.jpg", // data-src beats the src bait
		att.URL + "/placeholder.png",       // data: URI is skipped, src falls through
		att.URL + "/real-attachment-2.webp",
		att.URL + "/dup-1.jpg", // de-duplicated: only the first occurrence stays
		"https://other.example.net/pic.jpg",
	}
	if len(streams) != len(want) {
		t.Fatalf("got %d streams, want %d: %+v", len(streams), len(want), streams)
	}
	for i, st := range streams {
		if st.URL != want[i] {
			t.Errorf("stream[%d].URL = %s, want %s", i, st.URL, want[i])
		}
		if st.Kind != "image" || st.Label != "img" {
			t.Errorf("stream[%d] = Kind %q Label %q, want image/img", i, st.Kind, st.Label)
		}
	}
	// Same-host chrome (s) and non-image URLs must never appear.
	for _, st := range streams {
		if strings.HasPrefix(st.URL, s.URL) ||
			strings.HasSuffix(st.URL, "/video.mp4") ||
			strings.HasSuffix(st.URL, "/ad-bait.png") {
			t.Errorf("unwanted stream leaked: %s", st.URL)
		}
	}
}

// #4: extractImages surfaces a fetch failure instead of an empty list.
func TestExtractImagesFetchError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	c, err := NewClient("", "", defaultUA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.extractImages(s.URL + "/gone.html"); err == nil {
		t.Fatal("want an error for a non-200 page")
	}
}
