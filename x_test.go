package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsXURL(t *testing.T) {
	cases := []struct {
		u    string
		want bool
	}{
		{"https://x.com/NASA/status/123", true},
		{"https://twitter.com/NASA/status/123", true},
		{"https://mobile.twitter.com/NASA/status/123", true},
		{"https://www.x.com/NASA/status/123", true},
		{"https://x.com/", true},
		{"http://x.com/NASA/status/123?s=20", true},
		{"https://youtube.com/watch?v=abc", false},
		{"https://x.com.evil.example/NASA", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if got := isXURL(c.u); got != c.want {
			t.Errorf("isXURL(%q) = %v, want %v", c.u, got, c.want)
		}
	}
}

// TestExtractX exercises grouping, resolution ordering and codec labels on an
// x.com-style server-side rendered page. The m3u8 URL is entity-escaped
// (&amp;) to prove unescaping works, and one mp4 is duplicated to prove
// de-duplication.
func TestExtractX(t *testing.T) {
	page := `<!doctype html><html><body>
<script>window.__INITIAL_STATE__={"video_info":{"variants":[
{"bitrate":2176000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/2088355099089219585/vid/avc1/1280x720/9DP74OE5DErvDz_0.mp4?tag=14"},
{"bitrate":832000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/2088355099089219585/vid/avc1/640x360/CSZ304Vc1gip356S.mp4?tag=14"},
{"content_type":"application/x-mpegURL","url":"https://video.twimg.com/amplify_video/2088355099089219585/pl/FbKG5RJromPhDuWH.m3u8?tag=14&amp;v=2e4"},
{"bitrate":2176000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/2088355099089219585/vid/avc1/1280x720/9DP74OE5DErvDz_0.mp4?tag=14"}
]}}</script>
<script>quoted={"video_info":{"variants":[{"content_type":"video/mp4","url":"https://video.twimg.com/ext_tw_video/987654321/pu/vid/hvc1/1920x1080/QUOTED123.mp4?tag=12"}]}}</script>
<img src="https://video.twimg.com/tweet_video/HPtUZ-3XYAADLUG.mp4">
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(page))
	}))
	defer srv.Close()

	c, err := NewClient("", "", defaultUA, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := extractX(c, srv.URL+"/NASA/status/123")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		url, kind, label, group string
	}{
		{"vid/avc1/1280x720/9DP74OE5DErvDz_0.mp4", "direct", "h264", "2088355099089219585"},
		{"vid/avc1/640x360/CSZ304Vc1gip356S.mp4", "direct", "h264", "2088355099089219585"},
		{"pl/FbKG5RJromPhDuWH.m3u8?tag=14&v=2e4", "hls", "h264", "2088355099089219585"},
		{"vid/hvc1/1920x1080/QUOTED123.mp4", "direct", "h265", "987654321"},
		{"tweet_video/HPtUZ-3XYAADLUG.mp4", "direct", "h264", ""},
	}
	if len(streams) != len(want) {
		t.Fatalf("got %d streams, want %d: %+v", len(streams), len(want), streams)
	}
	for i, w := range want {
		s := streams[i]
		if !strings.Contains(s.URL, w.url) || s.Kind != w.kind || s.Label != w.label || s.Group != w.group {
			t.Errorf("stream %d = {%s kind=%s label=%s group=%s}, want url containing %s kind=%s label=%s group=%s",
				i, s.URL, s.Kind, s.Label, s.Group, w.url, w.kind, w.label, w.group)
		}
	}
}

func TestExtractXNoVideo(t *testing.T) {
	cases := []struct {
		page string
		want string
	}{
		{"<html><body>just a text tweet</body></html>", "推文中没有找到视频"},
		{`<html><body><img src="https://pbs.twimg.com/media/HPdX07VXsAA4bvl.jpg"></body></html>`, "没有视频（只有文字或图片）"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(c.page))
		}))
		client, err := NewClient("", "", defaultUA, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = extractX(client, srv.URL+"/NASA/status/456")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("page %q: expected error containing %q, got %v", c.page, c.want, err)
		}
	}
}
