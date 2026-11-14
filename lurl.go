package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// lurl.cc (https://lurl.cc/<id>) is a password-gated short-link host. Two
// facts make it workable without a browser:
//
//   - The `#form_password` + `/session.php` markup in every page is template
//     boilerplate: its presence proves nothing. A real gate has hidden inputs
//     `GETdomin` / `encrypt_pass` and no media `<source>` in the HTML.
//   - The unlock is a plain POST to /session.php; the only non-trivial field
//     is `pp_a`, an anti-bot hash computed by inline JS from `data-pp`
//     (an FNV-1a variant with 32-bit Math.imul multiplies), ported below as
//     computePpA.
//
// After a successful POST the server sets a session cookie; the same page
// re-fetched with that cookie contains the media `<source>` tags, which the
// normal extraction path already understands.

type lurlGate struct {
	id          string // GETdomin (the short id)
	encryptPass string
	dataPP      string // pp_a input's data-pp
	ppT         string // data-pt
	ppN         string // data-pn
}

var inputTagRe = regexp.MustCompile(`(?s)<input[^>]*>`)

// inputAttr extracts name="..." or value="..." (double-quoted) from one
// <input> tag.
func inputAttr(tag, attr string) (string, bool) {
	m := regexp.MustCompile(regexp.QuoteMeta(attr) + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// parseLurlGate returns the gate parameters, or nil when the page is not
// password-locked (template JS alone does not count).
func parseLurlGate(body []byte) *lurlGate {
	var g lurlGate
	for _, tag := range inputTagRe.FindAllString(string(body), -1) {
		if name, _ := inputAttr(tag, "name"); name == "encrypt_pass" {
			if v, ok := inputAttr(tag, "value"); ok && v != "" {
				g.encryptPass = v
			}
		}
		if name, _ := inputAttr(tag, "name"); name == "pp_a" {
			if v, ok := inputAttr(tag, "data-pp"); ok && v != "" {
				g.dataPP = v
			}
			if v, ok := inputAttr(tag, "data-pt"); ok {
				g.ppT = v
			}
			if v, ok := inputAttr(tag, "data-pn"); ok {
				g.ppN = v
			}
		}
		if name, _ := inputAttr(tag, "name"); name == "GETdomin" {
			if v, ok := inputAttr(tag, "value"); ok && v != "" {
				g.id = v
			}
		}
	}
	if g.encryptPass == "" || g.dataPP == "" {
		return nil
	}
	return &g
}

// computePpA ports the page's inline anti-bot JS: FNV-1a over data-pp where
// every character is re-read for 7*len iterations and mixed with the loop
// index; the multiply is Math.imul's 32-bit wrap (native uint32 in Go).
func computePpA(dataPP string) string {
	h := uint32(0x811c9dc5)
	n := len(dataPP)
	for i := 0; i < n*7; i++ {
		h ^= uint32(dataPP[i%n]) + uint32(i&0xff)
		h *= 16777619
		h ^= h >> 13
	}
	return fmt.Sprintf("%08x", h)
}

// unlockLurl attempts the lurl.cc password flow. It reports whether the page
// was actually gated; a nil error with gated=false simply means "not a
// password page" and the caller keeps its original extraction result.
// On success the client's cookie jar carries the session, so the re-fetch
// (and any media download) is authenticated.
func (c *Client) unlockLurl(pageURL, password string) (gated bool, err error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return false, err
	}
	if !strings.HasSuffix(strings.ToLower(u.Host), "lurl.cc") {
		return false, nil
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return false, err
	}
	c.http.Jar = jar

	body, err := c.GetBytes(pageURL)
	if err != nil {
		return false, fmt.Errorf("lurl.cc fetch: %w", err)
	}
	gate := parseLurlGate(body)
	if gate == nil {
		// Not gated: restore a jar-free client so nothing else changes.
		c.http.Jar = nil
		return false, nil
	}
	if password == "" {
		return true, fmt.Errorf("lurl.cc password gate on %s: re-run with -pass <password>", pageURL)
	}
	if gate.id == "" {
		gate.id = path.Base(u.Path)
	}
	gate.id = strings.TrimPrefix(gate.id, "/")

	form := url.Values{
		"GETdomin":     {gate.id},
		"password":     {password},
		"encrypt_pass": {gate.encryptPass},
		"pp_t":         {gate.ppT},
		"pp_n":         {gate.ppN},
		"pp_a":         {computePpA(gate.dataPP)},
	}
	sessionURL := u.Scheme + "://" + u.Host + "/session.php"
	req, err := http.NewRequest(http.MethodPost, sessionURL, strings.NewReader(form.Encode()))
	if err != nil {
		return true, err
	}
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("lurl.cc unlock POST: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return true, fmt.Errorf("lurl.cc unlock: unexpected response (HTTP %s)", resp.Status)
	}
	if out.Type != "SUCCESS" {
		return true, fmt.Errorf("lurl.cc unlock: %s", out.Text)
	}
	return true, nil
}
