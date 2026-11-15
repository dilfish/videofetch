package main

import "testing"

// #17: outName keeps mp4 for every non-image kind regardless of the source
// URL's extension — an HLS stream URL ends in .m3u8 and naming the output
// .m3u8 makes ffmpeg pick its HLS muxer (regression from fffea76, which let
// urlExt apply to all kinds and turned a 20-minute download into ~200 loose
// .ts segment files).
func TestOutName(t *testing.T) {
	cases := []struct {
		kind string
		url  string
		want string
	}{
		{"hls", "https://cdn.example.com/hls/1259497/index.m3u8", "abcde.1.mp4"},
		{"direct", "https://cdn.example.com/v/123.mp4?st=token", "abcde.1.mp4"},
		{"direct", "https://cdn.example.com/v/123.webm", "abcde.1.webm"}, // container ext is kept for direct streams
		{"direct", "https://cdn.example.com/v/123", "abcde.1.mp4"},
		{"image", "https://cdn.example.com/i/2026/a.webp", "abcde.1.webp"},
		{"image", "https://cdn.example.com/i/2026/a.JPEG", "abcde.1.jpg"},
		{"image", "https://cdn.example.com/i/2026/a.png", "abcde.1.png"},
	}
	for _, tc := range cases {
		if got := outName("abcde", 1, Stream{URL: tc.url, Kind: tc.kind}); got != tc.want {
			t.Errorf("outName(%s, %s) = %q, want %q", tc.kind, tc.url, got, tc.want)
		}
	}
}
