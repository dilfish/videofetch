package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// #8: percent-encoded document.write(strencode2(...)) sources are decoded
// and kept as streams, while commented-out decoy sources (91porn ships an
// unusable placeholder CDN URL inside an HTML comment) are dropped from the
// raw scan — the DOM walk already ignores comments, so the raw scan must not
// disagree.
func TestExtractStrencode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head></head><body>
<!-- <source src="https://ccm.91p52.com/358999.mp4?st=DECOY&f=placeholder" type='video/mp4'> -->
<script><!--
document.write(strencode2("%3c%73%6f%75%72%63%65%20%73%72%63%3d%27%68%74%74%70%73%3a%2f%2f%6c%61%2e%62%74%63%36%32%30%2e%63%6f%6d%2f%2f%6d%70%34%33%2f%31%32%35%39%35%37%39%2e%6d%70%34%3f%73%74%3d%4e%48%59%45%56%56%33%75%78%76%34%30%41%4f%32%42%52%6d%55%34%76%67%27%20%74%79%70%65%3d%27%76%69%64%65%6f%2f%6d%70%34%27%3e"));
document.write("%3c%73%6f%75%72%63%65%20%73%72%63%3d%27%68%74%74%70%73%3a%2f%2f%70%6c%61%69%6e%2e%65%78%61%6d%70%6c%65%2e%6f%72%67%2f%77%2e%6d%70%34%27%3e");
//--></script>
<video src="https://real.example.net/live.m3u8"></video>
</body></html>`)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	c, err := NewClient("", "", defaultUA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := c.extractStreams(s.URL+"/page.html", 0, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]Stream{}
	for _, st := range streams {
		got[st.URL] = st
	}
	want := map[string]string{
		// decoded from strencode2: uppercase hex in the source string
		"https://la.btc620.com//mp43/1259579.mp4?st=NHYEVV3uxv40AO2BRmU4vg": "strencode",
		// decoded from plain document.write: lowercase hex
		"https://plain.example.org/w.mp4": "strencode",
		// ordinary DOM source, untouched by the new passes
		"https://real.example.net/live.m3u8": "media tag",
	}
	if len(streams) != len(want) {
		t.Fatalf("got %d streams (%+v), want %d", len(streams), streams, len(want))
	}
	for u, label := range want {
		st, ok := got[u]
		if !ok {
			t.Errorf("missing stream %s; got %+v", u, streams)
			continue
		}
		if st.Label != label {
			t.Errorf("stream %s label = %q, want %q", u, st.Label, label)
		}
	}
	if _, ok := got["https://ccm.91p52.com/358999.mp4?st=DECOY&f=placeholder"]; ok {
		t.Error("commented-out decoy source leaked into the stream list")
	}
}

// #9: pctUnescape mirrors JS unescape semantics: %XX in both hex cases is
// decoded, '+' is never a space, and invalid or truncated escapes pass
// through unchanged instead of failing the whole string.
func TestPctUnescape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"%3C", "<"},
		{"%3c", "<"},
		{"abc", "abc"},
		{"a%2Fb", "a/b"},
		{"a+b", "a+b"},     // '+' is not a space (unlike QueryUnescape)
		{"a%zzb", "a%zzb"}, // invalid hex: passthrough
		{"%4", "%4"},       // truncated: passthrough
		{"100% off", "100% off"},
		{"%00", "\x00"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := pctUnescape(tc.in); got != tc.want {
			t.Errorf("pctUnescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
