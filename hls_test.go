package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errWriter is a writer that always fails, used to exercise the segment
// concatenation error path.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestFetchPlaylistToPreservesOrder stresses the out-of-order completion case:
// every segment is delayed by a different amount (later segments finish
// earlier), so only the index-keyed spool files can keep the output in
// playlist order. It also checks that spooled segment files are removed once
// they have been copied to dst.
func TestFetchPlaylistToPreservesOrder(t *testing.T) {
	const n = 6
	payloads := make([][]byte, n)
	delays := make([]time.Duration, n)

	mux := http.NewServeMux()
	mux.HandleFunc("/play.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:7\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.0,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	for i := 0; i < n; i++ {
		i := i
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 1000+i*137)
		delays[i] = time.Duration(n-i) * 25 * time.Millisecond
		mux.HandleFunc(fmt.Sprintf("/seg%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delays[i])
			w.Write(payloads[i])
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	segDir := filepath.Join(t.TempDir(), "segs")
	if err := os.Mkdir(segDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := fetchPlaylistTo(testClient(), srv.URL+"/play.m3u8", "best", n, segDir, &out); err != nil {
		t.Fatal(err)
	}

	var want []byte
	for _, p := range payloads {
		want = append(want, p...)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("segments were concatenated out of playlist order")
	}
	entries, err := os.ReadDir(segDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("spooled segment files were not removed after concatenation: %v", entries)
	}
}

// TestFetchPlaylistToFMP4 checks the EXT-X-MAP init section is written to dst
// before the media segments by the streaming implementation.
func TestFetchPlaylistToFMP4(t *testing.T) {
	srv := testServer(t)
	var out bytes.Buffer
	if err := fetchPlaylistTo(testClient(), srv.URL+"/hls/fmp4.m3u8", "best", 2, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, initPlain...), expectedConcat()...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("fMP4 stream mismatch:\ngot  %x\nwant %x", out.Bytes(), want)
	}
}

// TestFetchPlaylistToWorkersClamp covers the "clamp workers to at least one"
// change: a zero or negative concurrency must not deadlock on the semaphore.
func TestFetchPlaylistToWorkersClamp(t *testing.T) {
	srv := testServer(t)
	for _, workers := range []int{0, -3} {
		var out bytes.Buffer
		if err := fetchPlaylistTo(testClient(), srv.URL+"/hls/play.m3u8", "best", workers, t.TempDir(), &out); err != nil {
			t.Errorf("workers=%d: %v", workers, err)
			continue
		}
		if !bytes.Equal(out.Bytes(), expectedConcat()) {
			t.Errorf("workers=%d: stream mismatch", workers)
		}
	}
}

// TestFetchPlaylistToMasterPlaylist exercises the recursive master-playlist
// call, which must keep writing into the same segDir and dst.
func TestFetchPlaylistToMasterPlaylist(t *testing.T) {
	srv := testServer(t)
	segDir := t.TempDir()
	var out bytes.Buffer
	if err := fetchPlaylistTo(testClient(), srv.URL+"/hls/master.m3u8", "best", 4, segDir, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), expectedConcat()) {
		t.Error("master playlist variant selection produced wrong bytes")
	}
	if entries, _ := os.ReadDir(segDir); len(entries) != 0 {
		t.Errorf("master playlist left spool files behind: %v", entries)
	}
}

// TestFetchPlaylistToMissingSegDir ensures a failed spool write is reported and
// nothing is written to dst.
func TestFetchPlaylistToMissingSegDir(t *testing.T) {
	srv := testServer(t)
	var out bytes.Buffer
	err := fetchPlaylistTo(testClient(), srv.URL+"/hls/play.m3u8", "best", 2, filepath.Join(t.TempDir(), "no-such-dir"), &out)
	if err == nil {
		t.Fatal("expected an error when the spool directory does not exist")
	}
	if out.Len() != 0 {
		t.Errorf("partial output written despite spool failure: %d bytes", out.Len())
	}
}

// TestFetchPlaylistToWriteError covers the dst write failure path.
func TestFetchPlaylistToWriteError(t *testing.T) {
	srv := testServer(t)
	err := fetchPlaylistTo(testClient(), srv.URL+"/hls/play.m3u8", "best", 2, t.TempDir(), errWriter{})
	if err == nil || !strings.Contains(err.Error(), "write segment") {
		t.Fatalf("got %v, want a \"write segment\" error", err)
	}
}

// TestDownloadHLSCleanup verifies DownloadHLS leaves neither the .part file nor
// the spool directory behind on success.
func TestDownloadHLSCleanup(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.ts")
	if err := DownloadHLS(c, srv.URL+"/hls/play.m3u8", "best", out, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, expectedConcat()) {
		t.Error("downloaded file mismatch")
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("temp .part file was not cleaned up")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.ts" {
		t.Errorf("unexpected leftover files in output directory: %v", entries)
	}
}

// TestDownloadHLSFailureCleanup verifies a failed download removes the .part
// file and the spool directory instead of leaving debris behind.
func TestDownloadHLSFailureCleanup(t *testing.T) {
	srv := testServer(t)
	c := testClient()
	dir := t.TempDir()
	out := filepath.Join(dir, "fail.ts")
	if err := DownloadHLS(c, srv.URL+"/hls/missing.m3u8", "best", out, 2); err == nil {
		t.Fatal("expected an error for a missing playlist")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output file must not exist after a failed download")
	}
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf("temp .part file was not cleaned up after a failed download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed download left files behind: %v", entries)
	}
}
