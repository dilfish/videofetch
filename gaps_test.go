package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #10: streamInfo reports a direct stream's size in MB via HEAD, and an HLS
// stream's duration / segment count from the playlist without media
// download; "-" for unreachable sources on both paths.
func TestStreamInfoDirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/video.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "25000000")
		w.Write(make([]byte, 16))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	c := newTestClient("", "ua")

	if got := streamInfo(c, Stream{URL: s.URL + "/video.mp4", Kind: "direct"}, "best"); got != "25.0 MB" {
		t.Errorf("direct streamInfo = %q, want %q", got, "25.0 MB")
	}
	// "Unreachable" for direct streams means the HEAD request itself fails —
	// a dead server yields ContentLength -1 and streamInfo falls back to "-".
	// (A 404 with an empty body reports ContentLength 0, which displays as
	// "0.0 MB" — what the server actually said.)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if got := streamInfo(c, Stream{URL: deadURL + "/gone.mp4", Kind: "direct"}, "best"); got != "-" {
		t.Errorf("unreachable direct streamInfo = %q, want \"-\"", got)
	}
}

// #11: streamInfo for HLS reports duration and segment count from a plain
// media playlist (no key, no master): 3 segments x 4.0s = 0:12, no
// resolution line.
func TestStreamInfoHLS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/play.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.0,\nseg0.ts\n#EXTINF:4.0,\nseg1.ts\n#EXTINF:4.0,\nseg2.ts\n#EXT-X-ENDLIST\n")
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	c := newTestClient("", "ua")

	got := streamInfo(c, Stream{URL: s.URL + "/play.m3u8", Kind: "hls"}, "best")
	if !strings.Contains(got, "0:12") || !strings.Contains(got, "3 seg") {
		t.Errorf("hls streamInfo = %q, want duration 0:12 and \"3 seg\"", got)
	}
	if strings.Contains(got, "x") || strings.Contains(got, "RESOLUTION") {
		t.Errorf("hls streamInfo = %q, want no resolution without a master playlist", got)
	}
	if got := streamInfo(c, Stream{URL: s.URL + "/gone.m3u8", Kind: "hls"}, "best"); got != "-" {
		t.Errorf("failing hls streamInfo = %q, want \"-\"", got)
	}
}

// #12: download() routes by Kind — direct and image both stream to the file,
// an unknown kind is an error naming the kind.
func TestDownloadDispatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("MP4PAYLOAD"))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	c := newTestClient("", "ua")
	dir := t.TempDir()

	for _, kind := range []string{"direct", "image"} {
		out := filepath.Join(dir, kind+".bin")
		if err := download(c, Stream{URL: s.URL + "/v.mp4", Kind: kind}, out, 4, "best"); err != nil {
			t.Fatalf("download(kind=%s): %v", kind, err)
		}
		b, err := os.ReadFile(out)
		if err != nil || string(b) != "MP4PAYLOAD" {
			t.Errorf("download(kind=%s): out = %q, err = %v", kind, b, err)
		}
	}

	err := download(c, Stream{URL: s.URL + "/v.mp4", Kind: "weird"}, filepath.Join(dir, "x.bin"), 4, "best")
	if err == nil || !strings.Contains(err.Error(), "unknown stream kind \"weird\"") {
		t.Errorf("unknown kind: err = %v, want unknown-kind error", err)
	}
}

// #13: download() routes hls kinds through DownloadHLS: an fMP4 playlist
// (init section + fragments, no AES key) produces a non-empty output file
// regardless of whether the host has ffmpeg.
func TestDownloadDispatchHLS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/play.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.0,\nf0.m4s\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("INIT-SECTION"))
	})
	mux.HandleFunc("/f0.m4s", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("FRAG0-CONTENT"))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	c := newTestClient("", "ua")

	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := download(c, Stream{URL: s.URL + "/play.m3u8", Kind: "hls"}, out, 2, "best"); err != nil {
		t.Fatalf("download(kind=hls): %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("output file is empty")
	}
}

// #14: haveFFmpeg tracks PATH (no ffmpeg binary -> false, a shim -> true)
// and remux shells out to ffmpeg with the documented argument order
// (-y -i src -c copy dst): a shim copying $3 to $6 produces the output,
// a failing shim surfaces its stderr in the error.
func TestHaveFFmpegAndRemux(t *testing.T) {
	empty := t.TempDir()
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "ffmpeg"), []byte("#!/bin/sh\n/bin/cp \"$3\" \"$6\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	failShim := t.TempDir()
	if err := os.WriteFile(filepath.Join(failShim, "ffmpeg"), []byte("#!/bin/sh\necho boom-input >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", empty)
	if haveFFmpeg() {
		t.Error("haveFFmpeg = true with empty PATH, want false")
	}

	t.Setenv("PATH", shim)
	if !haveFFmpeg() {
		t.Error("haveFFmpeg = false with ffmpeg shim on PATH, want true")
	}
	src := filepath.Join(t.TempDir(), "src.ts")
	dst := filepath.Join(t.TempDir(), "dst.mp4")
	os.WriteFile(src, []byte("ts-bytes"), 0o644)
	if err := remux(src, dst); err != nil {
		t.Fatalf("remux with copying shim: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "ts-bytes" {
		t.Errorf("remux output = %q, err = %v, want copied source bytes", b, err)
	}

	t.Setenv("PATH", failShim)
	if err := remux(src, filepath.Join(t.TempDir(), "x.mp4")); err == nil || !strings.Contains(err.Error(), "boom-input") {
		t.Errorf("remux with failing shim: err = %v, want error containing shim stderr", err)
	}
}

// #15: parseIV accepts hex with or without the 0x prefix; anything else
// (wrong length, bad hex, empty) falls back to nil — the sequence-number
// default — rather than erroring.
func TestParseIV(t *testing.T) {
	full := strings.Repeat("ab", 16)
	cases := []struct {
		in   string
		want []byte
	}{
		{"0x" + full, make([]byte, 16)}, // presence, not content, is asserted below
		{full, make([]byte, 16)},
		{"0x" + strings.Repeat("ab", 15), nil}, // 15 bytes
		{"zz" + strings.Repeat("ab", 15), nil}, // bad hex
		{"", nil},
	}
	for i, tc := range cases {
		iv := parseIV(tc.in)
		if tc.want == nil {
			if iv != nil {
				t.Errorf("case %d parseIV(%q): iv = %v, want nil", i, tc.in, iv)
			}
			continue
		}
		if len(iv) != 16 {
			t.Errorf("case %d parseIV(%q): iv len %d, want 16", i, tc.in, len(iv))
		}
		if i == 0 && (iv[0] != 0xab || iv[15] != 0xab) {
			t.Errorf("case %d parseIV: 0x prefix not stripped: %v", i, iv)
		}
	}
}

// #16: a failing segment download surfaces as an error from
// DownloadHLS instead of a silent truncated file.
func TestDownloadHLSFailingSegment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/play.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.0,\ngood.ts\n#EXTINF:4.0,\nmissing.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/good.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ts-bytes"))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	c, err := NewClient("", "", defaultUA, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out.ts")
	if err := DownloadHLS(c, s.URL+"/play.m3u8", "best", out, 2); err == nil {
		t.Error("DownloadHLS with a 404 segment: want error, got nil")
	}
}
