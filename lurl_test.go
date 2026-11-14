package main

import "testing"

// Vector verified with the site's own inline JS (extracted verbatim and run
// in node 22): data-pp "cdfa0b86b7c65ad0" hashes to ee2ac13f. Note data-pp is
// regenerated on every page fetch, so vectors are only meaningful together
// with the exact input string.
func TestComputePpA(t *testing.T) {
	got := computePpA("cdfa0b86b7c65ad0")
	if got != "ee2ac13f" {
		t.Fatalf("computePpA = %q, want %q", got, "ee2ac13f")
	}
}

const lurlGatedPage = `<!doctype html><html><body>
<form id="form_password">
<input type="hidden" name="GETdomin" value="h3Uvq">
<input type="hidden" name="pp_t" value="1794646491">
<input type="hidden" name="pp_n" value="21ebc23c704f2228">
<input type="hidden" name="pp_a" value="" data-pp="cdfa0b86b7c65ad0" data-pt="1794646491" data-pn="21ebc23c704f2228">
<input type="hidden" name="encrypt_pass" value="eE1JTnBvWVRLQUZyMkkyblp6Ujc1Zz09OjqKG8omiMu1Nk3sOdplA/6k">
<input type="text" name="password" id="password" value="">
</form>
<script>$("#form_password").submit(function (e) {});</script>
</body></html>`

// The template JS references form_password/session.php on every page; only
// the actual hidden inputs constitute a gate.
const lurlUngatedPage = `<!doctype html><html><body>
<video class="video-js"><source src='https://r2limit2.lurl.cc/20261113/03b913f3680053c6d2bb3d7c6d097387.mp4'></video>
<script>$("#form_password").submit(function (e) {});</script>
</body></html>`

func TestParseLurlGate(t *testing.T) {
	g := parseLurlGate([]byte(lurlGatedPage))
	if g == nil {
		t.Fatal("gated page not detected")
	}
	if g.id != "h3Uvq" || g.ppT != "1794646491" || g.ppN != "21ebc23c704f2228" {
		t.Fatalf("gate fields wrong: %+v", g)
	}
	if g.encryptPass != "eE1JTnBvWVRLQUZyMkkyblp6Ujc1Zz09OjqKG8omiMu1Nk3sOdplA/6k" {
		t.Fatalf("encrypt_pass wrong: %q", g.encryptPass)
	}
	if g.dataPP != "cdfa0b86b7c65ad0" {
		t.Fatalf("data-pp wrong: %q", g.dataPP)
	}
	if parseLurlGate([]byte(lurlUngatedPage)) != nil {
		t.Fatal("ungated page misdetected as gated")
	}
}
