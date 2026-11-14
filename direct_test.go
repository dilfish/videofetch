package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyProgress(t *testing.T) {
	var dst bytes.Buffer
	n, err := copyProgress(&dst, strings.NewReader("hello"), 0, 5)
	if err != nil || n != 5 || dst.String() != "hello" {
		t.Errorf("copyProgress = (%d, %v, %q), want (5, nil, \"hello\")", n, err, dst.String())
	}

	// mid-read error: partial count is returned along with the error
	dst.Reset()
	n, err = copyProgress(&dst, io.MultiReader(strings.NewReader("ab"), errReader{}), 0, -1)
	if err == nil || n != 2 || dst.String() != "ab" {
		t.Errorf("copyProgress error path = (%d, %v, %q), want (2, err, \"ab\")", n, err, dst.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// TestDownloadDirectNoRangeOnFresh: a fresh download must not send Range.
func TestDownloadDirectNoRangeOnFresh(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("unexpected Range header %q on fresh download", r.Header.Get("Range"))
		}
		w.Write([]byte("0123456789"))
	}))
	defer s.Close()

	out := filepath.Join(t.TempDir(), "v.mp4")
	c := newTestClient("", "ua")
	if err := DownloadDirect(c, s.URL, out); err != nil {
		t.Fatalf("DownloadDirect: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "0123456789" {
		t.Errorf("file = %q, want full content", b)
	}
}

func TestDownloadDirectNon200(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer s.Close()

	out := filepath.Join(t.TempDir(), "v.mp4")
	err := DownloadDirect(newTestClient("", "ua"), s.URL, out)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("DownloadDirect: err = %v, want 404 error", err)
	}
}

// errBody returns a ReadCloser that yields s then fails mid-read, simulating
// a connection cut off while the body is streaming.
func errBody(s string) io.ReadCloser {
	return io.NopCloser(io.MultiReader(strings.NewReader(s), errReader{}))
}

// scriptedTripper serves canned responses so the interruption sequence is
// deterministic (a real aborted connection fails the whole request in Do).
type scriptedTripper struct {
	responses []*http.Response
	requests  []*http.Request
}

func (t *scriptedTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req)
	r := t.responses[0]
	t.responses = t.responses[1:]
	return r, nil
}

func newScriptedClient(responses ...*http.Response) *Client {
	return &Client{
		http: &http.Client{Transport: &scriptedTripper{responses: responses}},
		ua:   "ua",
	}
}

func fakeResp(status int, length int64, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode:    status,
		ContentLength: length,
		Body:          body,
		Header:        http.Header{},
	}
}

// TestDownloadDirectResume: the first connection is cut off after 5 bytes;
// the retry carries a Range header and the server serves the rest.
func TestDownloadDirectResume(t *testing.T) {
	c := newScriptedClient(
		fakeResp(http.StatusPartialContent, 10, errBody("01234")),
		fakeResp(http.StatusPartialContent, 5, io.NopCloser(strings.NewReader("56789"))),
	)
	out := filepath.Join(t.TempDir(), "v.mp4")
	if err := DownloadDirect(c, "http://x/v.mp4", out); err != nil {
		t.Fatalf("DownloadDirect: %v", err)
	}
	tr := c.http.Transport.(*scriptedTripper)
	if len(tr.requests) != 2 {
		t.Fatalf("attempts = %d, want 2", len(tr.requests))
	}
	if rg := tr.requests[1].Header.Get("Range"); rg != "bytes=5-" {
		t.Errorf("retry Range = %q, want bytes=5-", rg)
	}
	if b, _ := os.ReadFile(out); string(b) != "0123456789" {
		t.Errorf("file = %q, want resumed full content", b)
	}
}

// TestDownloadDirectRangeIgnored: on the retry the server answers 200 with
// the full body; the partial file must be truncated and rewritten.
func TestDownloadDirectRangeIgnored(t *testing.T) {
	c := newScriptedClient(
		fakeResp(http.StatusPartialContent, 10, errBody("01234")),
		fakeResp(http.StatusOK, 10, io.NopCloser(strings.NewReader("0123456789"))),
	)
	out := filepath.Join(t.TempDir(), "v.mp4")
	if err := DownloadDirect(c, "http://x/v.mp4", out); err != nil {
		t.Fatalf("DownloadDirect: %v", err)
	}
	tr := c.http.Transport.(*scriptedTripper)
	if rg := tr.requests[1].Header.Get("Range"); rg != "bytes=5-" {
		t.Errorf("retry Range = %q, want bytes=5-", rg)
	}
	if b, _ := os.ReadFile(out); string(b) != "0123456789" {
		t.Errorf("file = %q, want restarted full content (no appended partial)", b)
	}
}

// TestDirectSizeUnknown: HEAD failure yields -1 so callers can fall back
// to streaming size reporting.
func TestDirectSizeUnknown(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "123")
	}))
	defer s.Close()

	if got := directSize(newTestClient("", "ua"), s.URL); got != 123 {
		t.Errorf("directSize = %d, want 123", got)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if got := directSize(newTestClient("", "ua"), deadURL); got != -1 {
		t.Errorf("directSize on dead server = %d, want -1", got)
	}
}
