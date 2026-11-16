package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCoreAssetName pins the exact archive names: the release listing also
// carries -glibc/-musl/-legacy-macos-10.13 variants, and substring matching
// grabbed whichever of those came first.
func TestCoreAssetName(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"darwin", "amd64", "sing-box-1.14.4-darwin-amd64.tar.gz"},
		{"darwin", "arm64", "sing-box-1.14.4-darwin-arm64.tar.gz"},
		{"linux", "amd64", "sing-box-1.14.4-linux-amd64.tar.gz"},
		{"linux", "arm64", "sing-box-1.14.4-linux-arm64.tar.gz"},
		{"linux", "arm", "sing-box-1.14.4-linux-armv7.tar.gz"},
	}
	for _, c := range cases {
		if got := coreAssetName("1.14.4", c.goos, c.goarch); got != c.want {
			t.Errorf("coreAssetName(1.14.4, %s, %s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func withCoreURLs(t *testing.T, api, releases string) {
	t.Helper()
	oldAPI, oldRel := coreLatestAPI, coreReleasesURL
	coreLatestAPI, coreReleasesURL = api, releases
	t.Cleanup(func() { coreLatestAPI, coreReleasesURL = oldAPI, oldRel })
}

// TestLatestReleaseFallsBackToReleasePage covers the failing case reported in
// the wild: the anonymous API answers 403 (rate limit) and the old code
// reported it as "no release asset ... in tag " with an empty tag, because it
// never looked at the status code.
func TestLatestReleaseFallsBackToReleasePage(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for 203.0.113.7."}`)
	}))
	defer api.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/tag/v1.14.4", http.StatusFound)
	}))
	defer web.Close()
	withCoreURLs(t, api.URL, web.URL)

	tag, assets, err := latestRelease()
	if err != nil {
		t.Fatalf("latestRelease() error = %v, want the release page fallback", err)
	}
	if tag != "v1.14.4" {
		t.Errorf("tag = %q, want v1.14.4", tag)
	}
	if assets != nil {
		t.Errorf("assets = %v, want nil (the page only carries a tag)", assets)
	}
}

// TestLatestReleaseReportsBothFailures keeps the rate-limit cause visible when
// neither source works, instead of the old empty-tag message.
func TestLatestReleaseReportsBothFailures(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer api.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer web.Close()
	withCoreURLs(t, api.URL, web.URL)

	_, _, err := latestRelease()
	if err == nil {
		t.Fatal("latestRelease() succeeded, want an error naming both sources")
	}
	for _, want := range []string{"api:", "release page:", "429", "500"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestBootstrapCoreDownloadsPlainAsset runs the whole path against a fake
// release: the server only serves the plain archive, so fetching a variant
// (or asking the API's browser_download_url) fails the test.
func TestBootstrapCoreDownloadsPlainAsset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bootstrap ships .tar.gz only")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	const version = "9.9.9"
	tag := "v" + version
	asset := coreAssetName(version, runtime.GOOS, runtime.GOARCH)
	qualifier := fmt.Sprintf("sing-box-%s-%s-%s-glibc.tar.gz", version, runtime.GOOS, runtime.GOARCH)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			var assets []map[string]string
			for _, n := range []string{qualifier, asset} {
				assets = append(assets, map[string]string{"name": n})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
		case "/download/" + tag + "/" + asset:
			w.Write(tarGz(t, "sing-box-"+version+"-"+runtime.GOOS+"-"+runtime.GOARCH+"/sing-box",
				"#!/bin/sh\necho \"sing-box version "+version+"\"\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withCoreURLs(t, srv.URL+"/latest", srv.URL)

	dir := t.TempDir()
	bin, err := bootstrapCore(dir)
	if err != nil {
		t.Fatalf("bootstrapCore() error = %v", err)
	}
	if want := filepath.Join(dir, "bin", coreBinName); bin != want {
		t.Errorf("bin = %q, want %q", bin, want)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat %s: %v", bin, err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v)", bin, fi.Mode())
	}
}

// tarGz builds a single-file .tar.gz holding name with the given content.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
