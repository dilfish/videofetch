package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- safeName ---

func TestSafeName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a.mp4", true},
		{"视频 2026.mkv", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../a.mp4", false},
		{"a/b.mp4", false},
		{"a\\b.mp4", true}, // backslash is a legal file name char on unix
		{"./a.mp4", false},
		{"..a.mp4", true},
	}
	for _, c := range cases {
		if got := safeName(c.name); got != c.ok {
			t.Errorf("safeName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

// --- formatHost ---

func TestFormatHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"192.168.1.5", "192.168.1.5"},
		{"::1", "[::1]"},
		{"fe80::1", "[fe80::1]"},
	}
	for _, c := range cases {
		if got := formatHost(c.in); got != c.want {
			t.Errorf("formatHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- lanAddrs ---

func TestLanAddrs(t *testing.T) {
	addrs := lanAddrs()
	sawPrivate := false
	for _, a := range addrs {
		if strings.Contains(a, ":") && !strings.HasPrefix(a, "[") {
			t.Errorf("IPv6 address %q not bracketed", a)
		}
		// loopback and link-local must never appear
		if strings.HasPrefix(a, "127.") || strings.HasPrefix(a, "[fe80:") {
			t.Errorf("loopback/link-local address %q leaked", a)
		}
		// ordering: once we've seen anything after a private IPv4, no
		// further private IPv4 may appear
		if isPrivateIPv4Str(a) {
			if sawPrivate {
				t.Errorf("private IPv4 %q appears after non-private entry", a)
			}
			sawPrivate = true
		} else {
			sawPrivate = false
		}
	}
}

func isPrivateIPv4Str(s string) bool {
	if strings.Contains(s, ":") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	switch {
	case parts[0] == "10":
		return true
	case parts[0] == "192" && parts[1] == "168":
		return true
	case parts[0] == "172" && parts[1] >= "16" && parts[1] <= "31":
		return true
	}
	return false
}

// --- printURLs: capture log output for each branch ---

func capturePrintURLs(t *testing.T, addr, dir string) string {
	t.Helper()
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	printURLs(addr, dir)
	return buf.String()
}

func TestPrintURLs(t *testing.T) {
	if out := capturePrintURLs(t, "bad-addr", "/v"); !strings.Contains(out, "bad-addr") {
		t.Errorf("invalid addr: log = %q, want raw address fallback", out)
	}
	if out := capturePrintURLs(t, "127.0.0.1:8080", "/v"); !strings.Contains(out, "只监听了 127.0.0.1") {
		t.Errorf("loopback bind: log = %q, want loopback hint", out)
	}
	if out := capturePrintURLs(t, "192.168.1.5:8080", "/v"); !strings.Contains(out, "局域网访问: http://192.168.1.5:8080") {
		t.Errorf("specific bind: log = %q, want exact host URL", out)
	}
	if out := capturePrintURLs(t, ":8080", "/v"); !strings.Contains(out, "局域网访问:") {
		t.Errorf("wildcard bind: log = %q, want LAN URLs", out)
	}
	if out := capturePrintURLs(t, "[::1]:8080", "/v"); !strings.Contains(out, "只监听了 ::1") {
		t.Errorf("ipv6 loopback bind: log = %q, want loopback hint", out)
	}
}

// --- runServer error paths (no listen involved) ---

func TestRunServerDirErrors(t *testing.T) {
	if err := runServer("127.0.0.1:0", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("runServer with missing dir: want error, got nil")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	if err := runServer("127.0.0.1:0", file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("runServer with file as dir: want 'not a directory' error, got %v", err)
	}
}

// --- handlers, driven through the same mux as runServer ---

func newServeMux(t *testing.T, dir string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/videos", handleVideos(dir))
	mux.HandleFunc("/api/videos/", handleDeleteVideo(dir))
	mux.HandleFunc("/videos/", handleStream(dir))
	return mux
}

func setupVideoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.mp4")
	write("a.MKV")
	write("c.txt")     // not a video
	os.Mkdir(filepath.Join(dir, "d.mp4.dir"), 0o755) // dir with video ext, must be skipped
	return dir
}

func TestHandleIndex(t *testing.T) {
	mux := newServeMux(t, t.TempDir())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: code = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET /: content-type = %q, want text/html", ct)
	}
	if w.Body.Len() == 0 {
		t.Error("GET /: empty body, want embedded index.html")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /nope: code = %d, want 404", w.Code)
	}
}

func TestHandleVideos(t *testing.T) {
	mux := newServeMux(t, setupVideoDir(t))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/videos", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/videos: code = %d, want 200", w.Code)
	}
	var resp struct {
		Videos []videoInfo `json:"videos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	if len(resp.Videos) != 2 {
		t.Fatalf("got %d videos %+v, want 2 (a.MKV, b.mp4)", len(resp.Videos), resp.Videos)
	}
	if resp.Videos[0].Name != "a.MKV" || resp.Videos[1].Name != "b.mp4" {
		t.Errorf("videos not sorted by name: %s, %s", resp.Videos[0].Name, resp.Videos[1].Name)
	}
	if resp.Videos[0].URL != "/videos/a.MKV" {
		t.Errorf("url = %q, want /videos/a.MKV", resp.Videos[0].URL)
	}

	// POST is rejected
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/videos", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/videos: code = %d, want 405", w.Code)
	}
}

func TestHandleDeleteVideo(t *testing.T) {
	dir := setupVideoDir(t)
	mux := newServeMux(t, dir)

	// wrong method
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/videos/b.mp4", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET delete endpoint: code = %d, want 405", w.Code)
	}

	// path traversal rejected
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/videos/..%2F..%2Fetc", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("path traversal delete: code = %d, want 400", w.Code)
	}

	// not found
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/videos/gone.mp4", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("delete missing file: code = %d, want 404", w.Code)
	}

	// success removes the file
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/videos/b.mp4", nil))
	if w.Code != http.StatusNoContent {
		t.Errorf("delete b.mp4: code = %d, want 204", w.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.mp4")); !os.IsNotExist(err) {
		t.Error("b.mp4 still exists after delete")
	}
}

func TestHandleStream(t *testing.T) {
	dir := t.TempDir()
	content := "0123456789"
	if err := os.WriteFile(filepath.Join(dir, "v.mp4"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := newServeMux(t, dir)

	// full GET
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/videos/v.mp4", nil))
	if w.Code != http.StatusOK || w.Body.String() != content {
		t.Errorf("GET full: code = %d body = %q, want 200 %q", w.Code, w.Body.String(), content)
	}

	// Range request (player seeking)
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/videos/v.mp4", nil)
	req.Header.Set("Range", "bytes=3-5")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "345" {
		t.Errorf("GET range 3-5: code = %d body = %q, want 206 \"345\"", w.Code, w.Body.String())
	}

	// HEAD
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/videos/v.mp4", nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: code = %d bodyLen = %d, want 200 empty", w.Code, w.Body.Len())
	}

	// POST rejected
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/videos/v.mp4", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST stream: code = %d, want 405", w.Code)
	}

	// traversal rejected
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/videos/..%2F..%2Fetc%2Fpasswd", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("stream traversal: code = %d, want 400", w.Code)
	}

	// missing file
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/videos/gone.mp4", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("stream missing: code = %d, want 404", w.Code)
	}
}
