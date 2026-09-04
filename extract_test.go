package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testKey = "0x22222222222222222222222222222222"
)

var (
	segPlain = [][]byte{
		[]byte("first segment payload, not a multiple of 16 bytes"),
		[]byte("second segment payload with more content inside"),
		[]byte("third and final segment of the test stream"),
	}
	initPlain = []byte("FAKE-FTYP-MOOV-INIT-SECTION-BYTES")
)

func p7pad(b []byte) []byte {
	n := aes.BlockSize - len(b)%aes.BlockSize
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func addIV(iv []byte, n int) []byte {
	out := append([]byte{}, iv...)
	for i := 0; i < n; i++ {
		for j := 15; j >= 0; j-- {
			out[j]++
			if out[j] != 0 {
				break
			}
		}
	}
	return out
}

func aesEncrypt(plain, key, iv []byte) []byte {
	block, _ := aes.NewCipher(key)
	data := p7pad(plain)
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return out
}

// testServer serves a fake page with multiple stream sources, an AES-128
// encrypted HLS stream and a direct file — no external network involved.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	key := bytes.Repeat([]byte{0x11}, 16)
	baseIV := bytes.Repeat([]byte{0x22}, 16)

	mediaPlaylist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=%s
#EXTINF:4.0,
seg0.ts
#EXTINF:4.0,
seg1.ts
#EXTINF:4.0,
seg2.ts
#EXT-X-ENDLIST
`, testKey)

	masterPlaylist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720
play.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080
play_hi.m3u8
`

	fmp4Playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=%s
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
frag0.m4s
#EXTINF:4.0,
frag1.m4s
#EXTINF:4.0,
frag2.m4s
#EXT-X-ENDLIST
`, testKey)

	page := func(s *httptest.Server) string {
		return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<meta property="og:video" content="/direct.mp4">
<script type="application/ld+json">{"@type":"VideoObject","contentUrl":"%s/hls/play.m3u8"}</script>
</head><body>
<iframe src="/player/"></iframe>
<iframe src="https://evil.example/embed"></iframe>
<div class="dplayer" data-config='{"video":{"url":"/hls/play.m3u8","type":"hls"},"video_h265":"/hls/play_h265.m3u8"}'></div>
</body></html>`, s.URL)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/player/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<video src="/direct2.mp4"></video>`)
	})
	mux.HandleFunc("/direct.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "27") // len("FAKE-MP4-CONTENT-0123456789"), needed by HEAD
		w.Write([]byte("FAKE-MP4-CONTENT-0123456789"))
	})
	mux.HandleFunc("/direct2.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("FAKE-MP4-SECOND-9876543210"))
	})
	mux.HandleFunc("/hls/play.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mediaPlaylist)
	})
	mux.HandleFunc("/hls/play_hi.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mediaPlaylist)
	})
	mux.HandleFunc("/hls/play_h265.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, mediaPlaylist)
	})
	mux.HandleFunc("/hls/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, masterPlaylist)
	})
	mux.HandleFunc("/hls/key.bin", func(w http.ResponseWriter, r *http.Request) {
		w.Write(key)
	})
	mux.HandleFunc("/hls/fmp4.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fmp4Playlist)
	})
	mux.HandleFunc("/hls/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write(aesEncrypt(initPlain, key, baseIV))
	})
	for i := range segPlain {
		enc := aesEncrypt(segPlain[i], key, addIV(baseIV, i))
		mux.HandleFunc(fmt.Sprintf("/hls/seg%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
			w.Write(enc)
		})
		mux.HandleFunc(fmt.Sprintf("/hls/frag%d.m4s", i), func(w http.ResponseWriter, r *http.Request) {
			w.Write(enc)
		})
	}

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	// page.html needs the real server URL in JSON-LD; register it now.
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page(s))
	})
	return s
}

func testClient() *Client {
	c, err := NewClient("", "", defaultUA, 30*time.Second)
	if err != nil {
		panic(err)
	}
	return c
}

func expectedConcat() []byte {
	var b []byte
	for _, p := range segPlain {
		b = append(b, p...)
	}
	return b
}

func TestExtract(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	streams, err := c.extractStreams(srv.URL+"/page.html", 0, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ kind, label, path string }{
		{"direct", "og:video", "/direct.mp4"},
		{"direct", "media tag", "/direct2.mp4"},
		{"hls", "h264", "/hls/play.m3u8"}, // JSON-LD finds the same URL; h264 label wins
		{"hls", "h265", "/hls/play_h265.m3u8"},
	}
	found := map[string]Stream{}
	for _, s := range streams {
		found[s.URL] = s
		if strings.Contains(s.URL, "evil.example") {
			t.Errorf("cross-host iframe must not be followed: %s", s.URL)
		}
	}
	for _, w := range want {
		s, ok := found[srv.URL+w.path]
		if !ok {
			t.Errorf("missing stream %s", w.path)
			continue
		}
		if s.Kind != w.kind || s.Label != w.label {
			t.Errorf("%s: got kind=%s label=%s, want kind=%s label=%s", w.path, s.Kind, s.Label, w.kind, w.label)
		}
	}
	// JSON-LD duplicates the h264 URL; dedupe must collapse them.
	if len(streams) != 4 {
		t.Errorf("got %d streams after dedupe, want 4: %+v", len(streams), streams)
	}
}

func TestHLSMediaPlaylist(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	data, err := fetchPlaylist(c, srv.URL+"/hls/play.m3u8", "best", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, expectedConcat()) {
		t.Errorf("decrypted stream mismatch:\ngot  %x\nwant %x", data, expectedConcat())
	}
}

func TestHLSMasterPlaylist(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	data, err := fetchPlaylist(c, srv.URL+"/hls/master.m3u8", "best", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, expectedConcat()) {
		t.Errorf("master playlist variant selection produced wrong bytes")
	}
}

func TestHLSFMP4(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	data, err := fetchPlaylist(c, srv.URL+"/hls/fmp4.m3u8", "best", 4)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, initPlain...), expectedConcat()...)
	if !bytes.Equal(data, want) {
		t.Errorf("fMP4 output mismatch:\ngot  %x\nwant %x", data, want)
	}
	info, err := fetchHLSInfo(c, srv.URL+"/hls/fmp4.m3u8", "best")
	if err != nil {
		t.Fatal(err)
	}
	if info.Segments != 3 || info.Duration != 12.0 {
		t.Errorf("fMP4 info = %+v, want 3 segments / 12s", info)
	}
}

func TestFMP4UnsupportedVariants(t *testing.T) {
	base, _ := url.Parse("http://example.com/play.m3u8")
	initByterange := `#EXTM3U
#EXT-X-MAP:URI="init.mp4",BYTERANGE="0-100"
#EXTINF:4.0,
seg0.ts
`
	if _, _, _, err := parseMediaPlaylist(initByterange, base); err == nil || !strings.Contains(err.Error(), "BYTERANGE") {
		t.Errorf("init BYTERANGE: got %v, want BYTERANGE error", err)
	}
	segByterange := `#EXTM3U
#EXTINF:4.0,
#EXT-X-BYTERANGE:100@0
seg0.ts
`
	if _, _, _, err := parseMediaPlaylist(segByterange, base); err == nil || !strings.Contains(err.Error(), "BYTERANGE") {
		t.Errorf("segment BYTERANGE: got %v, want BYTERANGE error", err)
	}
}

func TestDownloadHLS(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	out := filepath.Join(t.TempDir(), "out.ts")
	if err := DownloadHLS(c, srv.URL+"/hls/play.m3u8", "best", out, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, expectedConcat()) {
		t.Errorf("downloaded file mismatch")
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("temp .part file was not cleaned up")
	}
}

func TestDownloadDirect(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := DownloadDirect(c, srv.URL+"/direct.mp4", out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "FAKE-MP4-CONTENT-0123456789" {
		t.Errorf("direct download content mismatch: %q", data)
	}
}

func TestHLSInfo(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	info, err := fetchHLSInfo(c, srv.URL+"/hls/play.m3u8", "best")
	if err != nil {
		t.Fatal(err)
	}
	if info.Segments != 3 || info.Duration != 12.0 {
		t.Errorf("media playlist info = %+v, want 3 segments / 12s", info)
	}
	info, err = fetchHLSInfo(c, srv.URL+"/hls/master.m3u8", "best")
	if err != nil {
		t.Fatal(err)
	}
	if info.Resolution != "1920x1080" || info.Bandwidth != 2000000 || info.Segments != 3 {
		t.Errorf("master info = %+v, want 1920x1080 @ 2Mbps / 3 segments", info)
	}
}

func TestDirectSize(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	if n := directSize(c, srv.URL+"/direct.mp4"); n != 27 {
		t.Errorf("directSize = %d, want 27", n)
	}
}

func TestListStreams(t *testing.T) {
	streams := []Stream{
		{URL: "https://x/a.m3u8", Kind: "hls", Label: "h264", Group: "player0"},
		{URL: "https://x/a_h265.m3u8", Kind: "hls", Label: "h265", Group: "player0"},
		{URL: "https://x/b.m3u8", Kind: "hls", Label: "h264", Group: "player1"},
		{URL: "https://x/og.mp4", Kind: "direct", Label: "og:video"},
	}
	got := listStreams(streams)
	if len(got) != 3 {
		t.Fatalf("listStreams = %d entries, want 3: %+v", len(got), got)
	}
	if got[0].URL != "https://x/a_h265.m3u8" || got[0].Label != "h265" {
		t.Errorf("player with both variants must show h265, got %+v", got[0])
	}
	if got[1].URL != "https://x/b.m3u8" {
		t.Errorf("player with only h264 must still show it, got %+v", got[1])
	}
	if got[2].URL != "https://x/og.mp4" {
		t.Errorf("standalone streams must remain, got %+v", got[2])
	}
}

func TestExtractGroups(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	streams, err := c.extractStreams(srv.URL+"/page.html", 0, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range streams {
		switch s.URL {
		case srv.URL + "/hls/play.m3u8":
			if s.Label != "h264" || s.Group != "player0" {
				t.Errorf("h264 stream = %+v, want label h264 group player0", s)
			}
		case srv.URL + "/hls/play_h265.m3u8":
			if s.Label != "h265" || s.Group != "player0" {
				t.Errorf("h265 stream = %+v, want label h265 group player0", s)
			}
		}
	}
}

func TestParsePicks(t *testing.T) {
	good := []struct {
		spec string
		n    int
		want []int
	}{
		{"1,3", 4, []int{1, 3}},
		{"1-3", 4, []int{1, 2, 3}},
		{"2,4-5", 6, []int{2, 4, 5}},
		{"3,1,3", 4, []int{1, 3}}, // de-duplicated and sorted
		{"1-1", 4, []int{1}},
	}
	for _, tc := range good {
		got, err := parsePicks(tc.spec, tc.n)
		if err != nil {
			t.Errorf("parsePicks(%q, %d): unexpected error %v", tc.spec, tc.n, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parsePicks(%q, %d) = %v, want %v", tc.spec, tc.n, got, tc.want)
		}
	}
	bad := []struct {
		spec string
		n    int
	}{
		{"", 4},
		{"0", 4},
		{"a", 4},
		{"3-1", 4},
		{"5", 4},
		{"1,", 4},
		{"1-", 4},
		{"-1", 4},
		{"1-5", 4},
	}
	for _, tc := range bad {
		if _, err := parsePicks(tc.spec, tc.n); err == nil {
			t.Errorf("parsePicks(%q, %d): expected error", tc.spec, tc.n)
		}
	}
}

func TestResolveVariant(t *testing.T) {
	streams := []Stream{
		{URL: "https://x/a.m3u8", Kind: "hls", Label: "h264", Group: "player0"},
		{URL: "https://x/a_h265.m3u8", Kind: "hls", Label: "h265", Group: "player0"},
		{URL: "https://x/b.m3u8", Kind: "hls", Label: "h264", Group: "player1"},
	}
	shown := listStreams(streams) // [a_h265, b]
	if got := resolveVariant(streams, shown[0], "best"); got.URL != "https://x/a_h265.m3u8" {
		t.Errorf("best → %s, want the shown h265 stream", got.URL)
	}
	if got := resolveVariant(streams, shown[0], "h264"); got.URL != "https://x/a.m3u8" {
		t.Errorf("h264 → %s, want a.m3u8", got.URL)
	}
	if got := resolveVariant(streams, shown[0], "h265"); got.URL != "https://x/a_h265.m3u8" {
		t.Errorf("h265 → %s, want a_h265.m3u8", got.URL)
	}
	// player without an h265 variant falls back to the shown stream
	if got := resolveVariant(streams, shown[1], "h265"); got.URL != "https://x/b.m3u8" {
		t.Errorf("missing variant → %s, want fallback b.m3u8", got.URL)
	}
}

func TestRandomName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]{5}$`)
	for i := 0; i < 50; i++ {
		if p := randomName(); !re.MatchString(p) {
			t.Fatalf("randomName() = %q, want 5 lowercase letters", p)
		}
	}
	if n := outName("abcde", 3); n != "abcde.3.mp4" {
		t.Errorf("outName = %q, want abcde.3.mp4", n)
	}
}

func TestParseAttrList(t *testing.T) {
	attrs := parseAttrList(`#EXT-X-KEY:METHOD=AES-128,URI="key file.bin",IV=0x1234`)
	if attrs["METHOD"] != "AES-128" || attrs["URI"] != "key file.bin" || attrs["IV"] != "0x1234" {
		t.Errorf("parseAttrList = %v", attrs)
	}
	attrs = parseAttrList(`#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080,NAME="1080p"`)
	if attrs["BANDWIDTH"] != "2000000" || attrs["RESOLUTION"] != "1920x1080" || attrs["NAME"] != "1080p" {
		t.Errorf("parseAttrList = %v", attrs)
	}
}
