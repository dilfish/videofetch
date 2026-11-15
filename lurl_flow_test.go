package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testDataPP      = "cdfa0b86b7c65ad0" // same vector as TestComputePpA
	testPPT         = "1794646491"
	testPPN         = "21ebc23c704f2228"
	testEncryptPass = "eE1JTnBvWVRLQUZyMkkyblp6Ujc1Zz09OjqKG8omiMu1Nk3sOdplA/6k"
)

func gatePage(gated bool) string {
	if !gated {
		return `<html><body><video><source src='https://media.example.org/v.mp4'></video></body></html>`
	}
	return fmt.Sprintf(`<html><body><form id="form_password">
<input type="hidden" name="GETdomin" value="abc123">
<input type="hidden" name="pp_t" value="%s"><input type="hidden" name="pp_n" value="%s">
<input type="hidden" name="pp_a" value="" data-pp="%s" data-pt="%s" data-pn="%s">
<input type="hidden" name="encrypt_pass" value="%s">
<input type="text" name="password" id="password">
</form></body></html>`, testPPT, testPPN, testDataPP, testPPT, testPPN, testEncryptPass)
}

// lurlTestServer serves the password flow: GET /page returns the gate page
// (or the unlocked page once the session cookie is set), POST /session.php
// records the submitted form and answers with a scripted response.
type lurlTestServer struct {
	srv      *httptest.Server
	posted   map[string]string
	postJSON string // raw JSON body scripted for the POST response
}

func newLurlTestServer(t *testing.T, postResponse func() string, ungated bool) *lurlTestServer {
	t.Helper()
	lt := &lurlTestServer{posted: map[string]string{}, postJSON: `{"type":"SUCCESS"}`}
	if postResponse != nil {
		lt.postJSON = ""
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		if ungated {
			fmt.Fprint(w, gatePage(false))
			return
		}
		if _, err := r.Cookie("lurlok"); err == nil {
			fmt.Fprint(w, gatePage(false))
			return
		}
		fmt.Fprint(w, gatePage(true))
	})
	mux.HandleFunc("/session.php", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		for _, k := range []string{"password", "encrypt_pass", "pp_a", "pp_t", "pp_n", "GETdomin"} {
			lt.posted[k] = r.Form.Get(k)
		}
		http.SetCookie(w, &http.Cookie{Name: "lurlok", Value: "1", Path: "/"})
		resp := `{"type":"SUCCESS"}`
		if postResponse != nil {
			resp = postResponse()
		}
		fmt.Fprint(w, resp)
	})
	lt.srv = httptest.NewServer(mux)
	t.Cleanup(lt.srv.Close)

	// point the site gate at the test server
	old := lurlHostSuffix
	u, err := url.Parse(lt.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	lurlHostSuffix = u.Host
	t.Cleanup(func() { lurlHostSuffix = old })
	return lt
}

// #19: the full unlock flow — GET the gated page, compute pp_a from
// data-pp, POST the form (urlencoded, encrypt_pass intact with its / and +),
// receive the session cookie, and re-fetch sees the unlocked page through
// the same client's jar.
func TestUnlockLurlFlowSuccess(t *testing.T) {
	lt := newLurlTestServer(t, nil, false)
	c, err := NewClient("", "", defaultUA, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	gated, err := c.unlockLurl(lt.srv.URL+"/page", "1113")
	if err != nil {
		t.Fatalf("unlockLurl: %v", err)
	}
	if !gated {
		t.Fatal("gated = false, want true")
	}

	if lt.posted["password"] != "1113" {
		t.Errorf("posted password = %q, want 1113", lt.posted["password"])
	}
	// encrypt_pass must survive URL encoding: it contains both '/' and '+'
	if lt.posted["encrypt_pass"] != testEncryptPass {
		t.Errorf("posted encrypt_pass = %q, want the raw value", lt.posted["encrypt_pass"])
	}
	if want := computePpA(testDataPP); lt.posted["pp_a"] != want {
		t.Errorf("posted pp_a = %q, want %q (computed from data-pp)", lt.posted["pp_a"], want)
	}
	if lt.posted["GETdomin"] != "abc123" {
		t.Errorf("posted GETdomin = %q, want abc123", lt.posted["GETdomin"])
	}
	if lt.posted["pp_t"] != testPPT || lt.posted["pp_n"] != testPPN {
		t.Errorf("posted pp_t/pp_n = %q/%q, want %s/%s", lt.posted["pp_t"], lt.posted["pp_n"], testPPT, testPPN)
	}

	if c.http.Jar == nil {
		t.Fatal("cookie jar not installed after unlock")
	}
	body, err := c.GetBytes(lt.srv.URL + "/page")
	if err != nil {
		t.Fatal(err)
	}
	// the re-fetch went through the jar, so the server served the unlocked page
	if !strings.Contains(string(body), "media.example.org") {
		t.Errorf("re-fetch after unlock still gated: %.120s", body)
	}
}

// #20: an ERROR-type reply surfaces its server text as the error.
func TestUnlockLurlErrorType(t *testing.T) {
	lt := newLurlTestServer(t, func() string { return `{"type":"ERROR","text":"wrong password"}` }, false)
	c, _ := NewClient("", "", defaultUA, 2*time.Second)
	if _, err := c.unlockLurl(lt.srv.URL+"/page", "1113"); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("unlockLurl: err = %v, want server text in error", err)
	}
}

// #21: a non-JSON reply (a Cloudflare block page) is reported as an
// unexpected response rather than silently treated as success.
func TestUnlockLurlNonJSON(t *testing.T) {
	lt := newLurlTestServer(t, func() string { return "<html>Attention Required! | Cloudflare</html>" }, false)
	c, _ := NewClient("", "", defaultUA, 2*time.Second)
	if _, err := c.unlockLurl(lt.srv.URL+"/page", "1113"); err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Errorf("unlockLurl: err = %v, want unexpected-response error", err)
	}
}

// #22: a page without the gate inputs is reported as not gated, and the
// client is left without a jar so the rest of the run is unchanged.
func TestUnlockLurlNotGated(t *testing.T) {
	lt := newLurlTestServer(t, nil, true)
	c, _ := NewClient("", "", defaultUA, 2*time.Second)
	gated, err := c.unlockLurl(lt.srv.URL+"/page", "1113")
	if err != nil {
		t.Fatalf("unlockLurl: %v", err)
	}
	if gated {
		t.Error("gated = true for an unlocked page")
	}
	if c.http.Jar != nil {
		t.Error("jar installed for an ungated page, want none")
	}
}

// #23: a gated page without a password fails fast with the -pass hint.
func TestUnlockLurlMissingPassword(t *testing.T) {
	lt := newLurlTestServer(t, nil, false)
	c, _ := NewClient("", "", defaultUA, 2*time.Second)
	if _, err := c.unlockLurl(lt.srv.URL+"/page", ""); err == nil || !strings.Contains(err.Error(), "-pass") {
		t.Errorf("unlockLurl without password: err = %v, want -pass hint", err)
	}
	if len(lt.posted) != 0 {
		t.Errorf("POST reached the server without a password: %+v", lt.posted)
	}
}
