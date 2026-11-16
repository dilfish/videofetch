// proxywrapper: subscription parser + proxy-core launcher for videofetch.
//
// It fetches a subscription, decodes the share links (vless / trojan / ss /
// vmess), picks a node, generates a config for a local proxy core
// (sing-box), starts the core, health-checks it, and prints the socks5
// address to feed videofetch -proxy. videofetch itself knows nothing about
// subscriptions or proxy protocols.
//
// Usage:
//
//	proxywrapper -list  -sub <URL>                     print nodes
//	proxywrapper       -sub <URL> [-pick hk] [-port 7890] [-pass pw]
//	proxywrapper -stop
//
// The core binary is taken from -core, else ~/.proxywrapper/bin/sing-box,
// else PATH; if missing it is downloaded from the official SagerNet/sing-box
// GitHub release for this GOOS/GOARCH.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

const (
	defaultPort   = 7890
	stateFile     = "state.json"
	coreBinName   = "sing-box"
	healthURL     = "http://www.gstatic.com/generate_204"
	subFetchUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	xrayVisionTag = "proxy"
)

// Node is one parsed share link.
type Node struct {
	Scheme string // vless / trojan / ss / vmess
	Name   string // fragment, shown in -list
	Host   string
	Port   int

	// shared
	UUID     string // vless/vmess id, trojan password
	SNI      string
	Insecure bool

	// ss
	Method string

	// vmess / transports
	Net      string // tcp / ws ("" = tcp)
	WSPath   string
	WSHost   string
	AlterID  int
	Security string // vmess encryption

	raw string
}

func (n Node) addr() string { return fmt.Sprintf("%s:%d", n.Host, n.Port) }

// --- subscription fetching and decoding ---

func fetchSubscription(subURL string) ([]byte, error) {
	// Local files: a plain existing path or file:// URL. Handy for keeping
	// the subscription out of shell history / process listings. The file
	// may hold either the subscription content itself or a single URL
	// pointing at it (a pointer file), which is then fetched over HTTP.
	if strings.HasPrefix(subURL, "file://") {
		subURL = strings.TrimPrefix(subURL, "file://")
	}
	if st, err := os.Stat(subURL); err == nil && st.Mode().IsRegular() {
		raw, err := os.ReadFile(subURL)
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(string(raw))
		if strings.Count(trimmed, "\n") == 0 && isHTTPURL(trimmed) {
			return fetchSubscription(trimmed) // pointer file -> fetch the URL
		}
		return raw, nil
	}
	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", subFetchUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", subURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// isHTTPURL reports whether s is a single http(s) URL.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// decodeSubscription accepts a plain share-link list or a base64-encoded one
// (padded or not, standard or URL-safe alphabet, possibly wrapped).
func decodeSubscription(raw []byte) ([]string, error) {
	text := strings.TrimSpace(string(raw))
	if strings.Contains(text, "://") {
		return splitLinks(text), nil
	}
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(text)
	for _, pad := range []string{"", "=", "==", "==="} {
		dec, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(compact, "=") + strings.TrimPrefix(pad, "="))
		if err != nil {
			dec, err = base64.StdEncoding.DecodeString(strings.TrimRight(compact, "=") + pad)
			if err != nil {
				continue
			}
		}
		s := string(dec)
		if strings.Contains(s, "://") {
			return splitLinks(s), nil
		}
	}
	return nil, fmt.Errorf("subscription is neither a link list nor base64-encoded links")
}

func splitLinks(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); strings.Contains(l, "://") {
			out = append(out, l)
		}
	}
	return out
}

// --- share-link parsing ---

func parseLinks(links []string) []Node {
	var nodes []Node
	for _, l := range links {
		n, err := parseLink(l)
		if err != nil {
			continue // skip malformed / placeholder entries silently
		}
		if n.Host == "" || n.Port == 0 || n.Host == "0.0.0.0" {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes
}

func parseLink(link string) (Node, error) {
	scheme, rest, ok := strings.Cut(link, "://")
	if !ok {
		return Node{}, fmt.Errorf("no scheme")
	}
	var n Node
	n.Scheme = strings.ToLower(scheme)
	n.raw = link

	if n.Scheme == "vmess" { // base64 JSON body, not a URL
		return parseVmess(rest)
	}

	// strip fragment (name) before URL parsing; it is not necessarily encoded
	name := ""
	if i := strings.LastIndex(rest, "#"); i >= 0 {
		name, rest = rest[i+1:], rest[:i]
	}
	n.Name, _ = url.QueryUnescape(name)

	u, err := url.Parse("scheme://" + strings.TrimPrefix(rest, "//"))
	if err != nil {
		return Node{}, err
	}
	if idx := strings.LastIndex(u.Host, ":"); idx >= 0 && !strings.Contains(u.Host, "]") {
		if p, err := strconv.Atoi(u.Host[idx+1:]); err == nil {
			n.Host, n.Port = u.Host[:idx], p
		}
	} else {
		n.Host = u.Host
		if n.Scheme == "vless" || n.Scheme == "trojan" {
			n.Port = 443
		}
	}
	n.UUID = u.User.Username()
	q := u.Query()
	n.SNI = q.Get("sni")
	if n.SNI == "" {
		n.SNI = q.Get("peer") // trojan's alias for sni
	}
	n.Insecure = q.Get("allowInsecure") == "1" || q.Get("allowInsecure") == "true"
	n.Net = q.Get("type")
	if n.Net == "" || n.Net == "tcp" {
		n.Net = "tcp"
	}
	n.WSPath = q.Get("path")
	n.WSHost = q.Get("host")
	n.Security = q.Get("encryption")

	switch n.Scheme {
	case "vless", "trojan":
		if n.UUID == "" {
			return Node{}, fmt.Errorf("empty %s credential", n.Scheme)
		}
	case "ss":
		if err := parseSS(n, u, rest); err != nil {
			return Node{}, err
		}
		return n, nil
	default:
		return Node{}, fmt.Errorf("unsupported scheme %q", n.Scheme)
	}
	return n, nil
}

// parseSS handles both ss://base64(method:pass)@host:port and the fully
// encoded ss://base64(method:pass@host:port) forms.
func parseSS(n Node, u *url.URL, rest string) error {
	if n.UUID == "" || n.Host == "" {
		// everything (method:pass@host:port) is in one base64 blob
		blob := strings.TrimPrefix(rest, "//")
		if i := strings.IndexAny(blob, "/?#"); i >= 0 {
			blob = blob[:i]
		}
		dec, err := base64.RawURLEncoding.DecodeString(blob)
		if err != nil {
			dec, err = base64.StdEncoding.DecodeString(blob)
			if err != nil {
				return err
			}
		}
		mp, hostport, ok := strings.Cut(string(dec), "@")
		if !ok {
			return fmt.Errorf("ss blob without @")
		}
		method, pass, _ := strings.Cut(mp, ":")
		n.Method, n.UUID = method, pass
		host, port, err := net.SplitHostPort(hostport)
		if err != nil {
			return err
		}
		n.Host = host
		n.Port, _ = strconv.Atoi(port)
		return nil
	}
	mp, _ := url.QueryUnescape(n.UUID)
	n.Method, n.UUID, _ = strings.Cut(mp, ":")
	if n.Method == "" || n.UUID == "" {
		return fmt.Errorf("ss link without method:password")
	}
	return nil
}

func parseVmess(body string) (Node, error) {
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
	if err != nil {
		dec, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(body))
		if err != nil {
			return Node{}, err
		}
	}
	var v struct {
		PS, Add, Port, ID, Aid, Net, Type, Host, Path, TLS, SNI, Scy string
	}
	if err := json.Unmarshal([]byte(string(dec)), &v); err != nil {
		return Node{}, err
	}
	if v.Add == "" || v.ID == "" {
		return Node{}, fmt.Errorf("vmess without add/id")
	}
	port, _ := strconv.Atoi(v.Port)
	n := Node{Scheme: "vmess", Name: v.PS, Host: v.Add, Port: port, UUID: v.ID, Net: v.Net, WSPath: v.Path, WSHost: v.Host, SNI: v.SNI, Security: v.Scy}
	if n.Security == "" {
		n.Security = "auto"
	}
	if n.Net == "" {
		n.Net = "tcp"
	}
	n.AlterID, _ = strconv.Atoi(v.Aid)
	return n, nil
}

// --- sing-box config generation ---

func singboxConfig(n Node, port int, pass string) (map[string]any, error) {
	tlsObj := func() map[string]any {
		t := map[string]any{"enabled": true}
		if n.SNI != "" && n.SNI != "0.0.0.0" {
			t["server_name"] = n.SNI
		}
		if n.Insecure {
			t["insecure"] = true
		}
		return t
	}

	var outbound map[string]any
	switch n.Scheme {
	case "vless":
		outbound = map[string]any{
			"type": "vless", "tag": xrayVisionTag,
			"server": n.Host, "server_port": n.Port,
			"uuid": n.UUID,
		}
		if flow := fragmentParam(n.raw, "flow"); flow != "" {
			outbound["flow"] = flow
		}
		outbound["tls"] = tlsObj()
	case "trojan":
		outbound = map[string]any{
			"type": "trojan", "tag": xrayVisionTag,
			"server": n.Host, "server_port": n.Port,
			"password": n.UUID, "tls": tlsObj(),
		}
	case "ss":
		outbound = map[string]any{
			"type": "shadowsocks", "tag": xrayVisionTag,
			"server": n.Host, "server_port": n.Port,
			"method": n.Method, "password": n.UUID,
		}
	case "vmess":
		outbound = map[string]any{
			"type": "vmess", "tag": xrayVisionTag,
			"server": n.Host, "server_port": n.Port,
			"uuid": n.UUID, "security": n.Security, "alter_id": n.AlterID,
		}
	default:
		return nil, fmt.Errorf("no sing-box mapping for scheme %q", n.Scheme)
	}

	if n.Net == "ws" {
		transport := map[string]any{"type": "ws", "path": n.WSPath}
		if n.WSHost != "" {
			transport["headers"] = map[string]any{"Host": n.WSHost}
		}
		outbound["transport"] = transport
	} else if n.Net != "tcp" {
		return nil, fmt.Errorf("transport %q not supported (tcp/ws only)", n.Net)
	}

	inbound := map[string]any{
		"type": "socks", "tag": "socks-in",
		"listen": "127.0.0.1", "listen_port": port,
	}
	if pass != "" {
		user, pw, _ := strings.Cut(pass, ":")
		inbound["users"] = []map[string]any{{"username": user, "password": pw}}
	}

	return map[string]any{
		"log":       map[string]any{"level": "warn"},
		"inbounds":  []map[string]any{inbound},
		"outbounds": []map[string]any{outbound, {"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"final": xrayVisionTag, "auto_detect_interface": true},
	}, nil
}

// fragmentParam re-reads a query parameter from the raw link: sing-box config
// generation needs params the URL parser already consumed (e.g. flow on
// vless), and re-parsing the fragment-stripped URL is safer than guessing.
func fragmentParam(raw, key string) string {
	i := strings.Index(raw, "?")
	if i < 0 {
		return ""
	}
	q := raw[i+1:]
	if j := strings.IndexAny(q, "#"); j >= 0 {
		q = q[:j]
	}
	for _, kv := range strings.Split(q, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			v, _ = url.QueryUnescape(v)
			return v
		}
	}
	return ""
}

// --- core management ---

// URL bases, variables so tests can point them at a local server.
var (
	coreReleasesURL = "https://github.com/SagerNet/sing-box/releases"
	coreLatestAPI   = "https://api.github.com/repos/SagerNet/sing-box/releases/latest"
)

// coreAssetName is the official plain archive for a platform:
// sing-box-<version>-<os>-<arch>.tar.gz. Releases also carry qualifier builds
// (-glibc, -musl, -softfloat, -legacy-macos-10.13) whose names come first in
// the asset listing, so the plain archive has to be named exactly instead of
// found by substring, which is what picked the wrong one before.
func coreAssetName(version, goos, goarch string) string {
	arch := goarch
	if goarch == "arm" {
		// sing-box ships armv5/armv6/armv7; armv7 is the GOARM default.
		arch = "armv7"
	}
	return fmt.Sprintf("sing-box-%s-%s-%s.tar.gz", version, goos, arch)
}

// latestRelease resolves the newest release tag and, when the GitHub API
// answers, the names of its assets. The anonymous API allows 60 requests per
// hour, so a 403 from a shared egress IP is routine rather than exceptional;
// the release page redirect carries the same tag and is not rate-limited, so
// it is the fallback. Callers that only get a tag can still build asset URLs
// from the naming convention.
func latestRelease() (tag string, assets []string, err error) {
	tag, assets, apiErr := latestReleaseAPI()
	if tag != "" {
		return tag, assets, nil
	}
	tag, webErr := latestTagFromWeb()
	if tag != "" {
		fmt.Fprintf(os.Stderr, "bootstrap: github api unusable (%v); using release page tag %s\n", apiErr, tag)
		return tag, nil, nil
	}
	return "", nil, fmt.Errorf("cannot resolve the latest sing-box release: api: %v; release page: %v", apiErr, webErr)
}

func latestReleaseAPI() (string, []string, error) {
	req, err := http.NewRequest("GET", coreLatestAPI, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", subFetchUA)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("GET %s: %s: %s", coreLatestAPI, resp.Status, bodySnippet(body))
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", coreLatestAPI, err)
	}
	if rel.TagName == "" {
		return "", nil, fmt.Errorf("%s: response has no tag_name: %s", coreLatestAPI, bodySnippet(body))
	}
	names := make([]string, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		names = append(names, a.Name)
	}
	return rel.TagName, names, nil
}

// latestTagFromWeb follows one hop of the release page redirect
// (.../releases/latest -> .../releases/tag/<tag>) and returns the tag.
func latestTagFromWeb() (string, error) {
	req, err := http.NewRequest("GET", coreReleasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", subFetchUA)
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("GET %s/latest: %s, no redirect to a tag (Location %q)", coreReleasesURL, resp.Status, loc)
	}
	tag := strings.Trim(loc[i+len("/tag/"):], "/")
	if tag == "" {
		return "", fmt.Errorf("GET %s/latest: empty tag in Location %q", coreReleasesURL, loc)
	}
	return tag, nil
}

// bodySnippet keeps error messages readable when a server answers with HTML.
func bodySnippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func bootstrapCore(dir string) (string, error) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	fmt.Fprintln(os.Stderr, "bootstrap: resolving the latest sing-box release for", goos+"/"+goarch)
	tag, assets, err := latestRelease()
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(tag, "v")
	asset := coreAssetName(version, goos, goarch)
	if len(assets) > 0 && !slices.Contains(assets, asset) {
		return "", fmt.Errorf("sing-box %s publishes no %s asset (see %s/tag/%s); pass -core with a binary you already have", tag, asset, coreReleasesURL, tag)
	}
	assetURL := coreReleasesURL + "/download/" + tag + "/" + asset
	fmt.Fprintln(os.Stderr, "bootstrap: fetching", assetURL)
	resp2, err := (&http.Client{Timeout: 5 * time.Minute}).Get(assetURL)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", assetURL, resp2.Status)
	}
	tmpTar, err := os.CreateTemp(dir, "sing-box-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpTar.Name())
	if _, err := io.Copy(tmpTar, resp2.Body); err != nil {
		tmpTar.Close()
		return "", err
	}
	tmpTar.Close()

	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return "", err
	}
	// tar xf into a scratch dir, then move the binary in place
	scratch, err := os.MkdirTemp(dir, "extract-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	if out, err := exec.Command("tar", "xf", tmpTar.Name(), "-C", scratch).CombinedOutput(); err != nil {
		return "", fmt.Errorf("tar: %v: %s", err, out)
	}
	bin := filepath.Join(dir, "bin", coreBinName)
	err = filepath.Walk(scratch, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(path) == coreBinName {
			return os.Rename(path, bin)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return "", err
	}
	// A binary that will not execute here means this platform's asset was
	// not the one we wanted; report that instead of failing later at run.
	if out, err := exec.Command(bin, "version").CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s did not run after download: %v: %s", bin, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintln(os.Stderr, "bootstrap: sing-box", tag, "->", bin)
	return bin, nil
}

func workDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".proxywrapper"), nil
}

func corePath(coreFlag string) (string, error) {
	if coreFlag != "" {
		if _, err := os.Stat(coreFlag); err != nil {
			return "", fmt.Errorf("-core %s: %w", coreFlag, err)
		}
		return coreFlag, nil
	}
	dir, err := workDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "bin", coreBinName)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" {
			continue
		}
		cand := filepath.Join(d, coreBinName)
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("sing-box not found (looked in %s and PATH); run with -bootstrap to download it", p)
}

func writeConfig(dir string, n Node, port int, pass string) (string, error) {
	cfg, err := singboxConfig(n, port, pass)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "config.json")
	return p, os.WriteFile(p, b, 0o600)
}

func startCore(coreBin, cfgPath, logPath string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(coreBin, "run", "-c", cfgPath)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return 0, err
	}
	return cmd.Process.Pid, logf.Close()
}

func waitPort(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// healthCheck performs an HTTP request through the local socks5 proxy.
func healthCheck(port int, pass string) (string, error) {
	var auth *proxy.Auth
	if pass != "" {
		user, pw, _ := strings.Cut(pass, ":")
		auth = &proxy.Auth{User: user, Password: pw}
	}
	d, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), auth, proxy.Direct)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: d.(proxy.ContextDialer).DialContext},
		Timeout:   15 * time.Second,
	}
	resp, err := client.Get(healthURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.Status, nil
}

type state struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Node    string `json:"node"`
	CoreBin string `json:"core_bin"`
	Started string `json:"started"`
	Pass    string `json:"pass,omitempty"`
}

func readState(dir string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeState(dir string, st state) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(filepath.Join(dir, stateFile), b, 0o600)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// showStatus reports the running core's recorded state plus live probes:
// the health check through the proxy and the exit IP's geolocation.
func showStatus(dir string) error {
	st, err := readState(dir)
	if err != nil {
		return fmt.Errorf("no running proxywrapper state (start one with -sub)")
	}
	alive := processAlive(st.PID)
	fmt.Printf("core:   pid %d, %s\n", st.PID, map[bool]string{true: "running", false: "DEAD"}[alive])
	fmt.Printf("node:   %s\n", st.Node)
	fmt.Printf("local:  socks5://127.0.0.1:%d (started %s)\n", st.Port, st.Started)
	fmt.Printf("bin:    %s\n", st.CoreBin)
	if !alive {
		return fmt.Errorf("core process is dead — state is stale; -stop to clean, then start again with -sub")
	}
	if !waitPort(st.Port, time.Second) {
		return fmt.Errorf("port %d is not listening although pid %d is alive", st.Port, st.PID)
	}
	status, err := healthCheck(st.Port, st.Pass)
	if err != nil {
		return fmt.Errorf("health check failed: %v", err)
	}
	fmt.Printf("health: %s\n", status)
	if geo, err := exitGeo(st.Port, st.Pass); err == nil {
		fmt.Printf("exit:   %s\n", geo)
	}
	return nil
}

// exitGeo asks a geolocation endpoint through the proxy for the exit IP.
func exitGeo(port int, pass string) (string, error) {
	var auth *proxy.Auth
	if pass != "" {
		user, pw, _ := strings.Cut(pass, ":")
		auth = &proxy.Auth{User: user, Password: pw}
	}
	d, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), auth, proxy.Direct)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: d.(proxy.ContextDialer).DialContext},
		Timeout:   15 * time.Second,
	}
	resp, err := client.Get("https://api.ip.sb/geoip")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var g struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		City    string `json:"city"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%s/%s)", g.IP, g.Country, g.City), nil
}

func stopCore(dir string) error {
	st, err := readState(dir)
	if err != nil {
		return fmt.Errorf("no running proxywrapper state: %w", err)
	}
	if !processAlive(st.PID) {
		os.Remove(filepath.Join(dir, stateFile))
		return fmt.Errorf("core pid %d not running (state cleaned)", st.PID)
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processAlive(st.PID) {
		time.Sleep(200 * time.Millisecond)
	}
	if processAlive(st.PID) {
		syscall.Kill(st.PID, syscall.SIGKILL)
	}
	os.Remove(filepath.Join(dir, stateFile))
	fmt.Printf("stopped pid %d (node %q, port %d)\n", st.PID, st.Node, st.Port)
	return nil
}

// --- CLI ---

func main() {
	var (
		sub     = flag.String("sub", "", "subscription URL")
		pick    = flag.String("pick", "", "node selection: 1-based index or case-insensitive name substring")
		port    = flag.Int("port", defaultPort, "local socks5 listen port")
		pass    = flag.String("pass", "", "proxy auth user:pass for the local socks5 inbound")
		core    = flag.String("core", "", "path to the sing-box binary (default: ~/.proxywrapper/bin/sing-box or PATH)")
		boot    = flag.Bool("bootstrap", false, "download sing-box from the official release if missing")
		list    = flag.Bool("list", false, "list parsed nodes and exit")
		stop    = flag.Bool("stop", false, "stop the running core")
		status  = flag.Bool("status", false, "show the running core's state and live health")
		keepLog = flag.Bool("log", false, "print the core log path and keep the core attached to this tty (foreground)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "proxywrapper: subscription -> local socks5 proxy for videofetch\n\n")
		fmt.Fprintf(os.Stderr, "usage: proxywrapper -sub <url> [-pick <idx|name>] [-port %d] [-pass user:pass]\n", defaultPort)
		fmt.Fprintf(os.Stderr, "       proxywrapper -list -sub <url>\n")
		fmt.Fprintf(os.Stderr, "       proxywrapper -status\n")
		fmt.Fprintf(os.Stderr, "       proxywrapper -stop\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir, err := workDir()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err)
	}

	if *stop {
		if err := stopCore(dir); err != nil {
			fatal(err)
		}
		return
	}
	if *status {
		if err := showStatus(dir); err != nil {
			fatal(err)
		}
		return
	}
	if *sub == "" {
		flag.Usage()
		os.Exit(2)
	}

	raw, err := fetchSubscription(*sub)
	if err != nil {
		fatal(err)
	}
	links, err := decodeSubscription(raw)
	if err != nil {
		fatal(err)
	}
	nodes := parseLinks(links)
	if len(nodes) == 0 {
		fatal(errors.New("no usable nodes in the subscription"))
	}

	if *list {
		fmt.Printf("%-4s %-8s %s\n", "#", "scheme", "name")
		for i, n := range nodes {
			fmt.Printf("[%d] %-8s %s (%s)\n", i+1, n.Scheme, n.Name, n.addr())
		}
		return
	}

	node := selectNode(nodes, *pick)
	if node == nil {
		fatal(fmt.Errorf("no node matches -pick %q; run -list to see candidates", *pick))
	}
	fmt.Printf("node: [%s] %s (%s)\n", node.Scheme, node.Name, node.addr())

	// a previous run's core must go before the port can be reused
	if st, err := readState(dir); err == nil && processAlive(st.PID) {
		fmt.Fprintf(os.Stderr, "stopping previous core (pid %d)\n", st.PID)
		if err := stopCore(dir); err != nil {
			fatal(err)
		}
	}

	coreBin, err := corePath(*core)
	if err != nil {
		if !*boot {
			fatal(err)
		}
		if coreBin, err = bootstrapCore(dir); err != nil {
			fatal(err)
		}
	}

	cfgPath, err := writeConfig(dir, *node, *port, *pass)
	if err != nil {
		fatal(err)
	}

	var pid int
	if *keepLog {
		cmd := exec.Command(coreBin, "run", "-c", cfgPath)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fatal(err)
		}
		pid = cmd.Process.Pid
	} else {
		logPath := filepath.Join(dir, fmt.Sprintf("core-%d.log", time.Now().Unix()))
		pid, err = startCore(coreBin, cfgPath, logPath)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "core log: %s\n", logPath)
	}

	if !waitPort(*port, 15*time.Second) {
		syscall.Kill(pid, syscall.SIGKILL)
		logFile := filepath.Join(dir, "core-latest.log")
		os.Rename(latestLog(dir), logFile)
		b, _ := os.ReadFile(logFile)
		fatal(fmt.Errorf("core did not open port %d; log tail: %s", *port, logTail(string(b))))
	}

	hStatus, err := healthCheck(*port, *pass)
	if err != nil {
		syscall.Kill(pid, syscall.SIGKILL)
		fatal(fmt.Errorf("proxy health check failed: %v", err))
	}

	auth := ""
	if *pass != "" {
		auth = *pass + "@"
	}
	if err := writeState(dir, state{PID: pid, Port: *port, Node: node.Name, CoreBin: coreBin, Started: time.Now().Format(time.RFC3339), Pass: *pass}); err != nil {
		fatal(err)
	}
	fmt.Printf("proxy up (pid %d, health %s)\n", pid, hStatus)
	fmt.Printf("videofetch -proxy socks5://%s127.0.0.1:%d ...\n", auth, *port)
}

func latestLog(dir string) string {
	entries, _ := os.ReadDir(dir)
	best, bestMod := "", time.Time{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "core-") && strings.HasSuffix(e.Name(), ".log") {
			if info, err := e.Info(); err == nil && info.ModTime().After(bestMod) {
				best, bestMod = filepath.Join(dir, e.Name()), info.ModTime()
			}
		}
	}
	return best
}

func logTail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, " | ")
}

func selectNode(nodes []Node, pick string) *Node {
	if pick == "" {
		return &nodes[0]
	}
	if idx, err := strconv.Atoi(pick); err == nil {
		if idx >= 1 && idx <= len(nodes) {
			return &nodes[idx-1]
		}
		return nil
	}
	needle := strings.ToLower(pick)
	for i := range nodes {
		if strings.Contains(strings.ToLower(nodes[i].Name), needle) ||
			strings.Contains(strings.ToLower(nodes[i].Host), needle) {
			return &nodes[i]
		}
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "proxywrapper: %v\n", err)
	os.Exit(1)
}
