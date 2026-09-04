package main

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// xVideoRe matches video file URLs inside x.com's server-side rendered HTML.
// Logged-out tweet pages embed the full variant list (every mp4 resolution
// plus the HLS master playlist) as JSON, with no authentication required.
var xVideoRe = regexp.MustCompile(`https://video\.twimg\.com/[A-Za-z0-9/._-]+\.(?:mp4|m3u8)(?:\?[^"'\s<>\\]*)?`)

// isXURL reports whether u points at a tweet on x.com or twitter.com.
func isXURL(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch strings.ToLower(pu.Host) {
	case "x.com", "twitter.com", "mobile.twitter.com", "www.x.com", "www.twitter.com":
		return true
	}
	return false
}

// extractX extracts video streams from an x.com tweet page.
func extractX(c *Client, tweetURL string) ([]Stream, error) {
	body, err := c.GetBytes(tweetURL)
	if err != nil {
		return nil, err
	}
	// URLs in HTML attributes come back entity-escaped (&amp; etc.).
	raw := html.UnescapeString(string(body))

	groups := map[string][]Stream{}
	var order []string
	for _, u := range xVideoRe.FindAllString(raw, -1) {
		key, kind, label := xStreamMeta(u)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], Stream{URL: u, Kind: kind, Label: label, Group: key})
	}

	var streams []Stream
	for _, key := range order {
		g := groups[key]
		if key != "" {
			// One video group: mp4 variants by resolution desc, HLS last.
			sort.SliceStable(g, func(i, j int) bool { return xRank(g[i]) > xRank(g[j]) })
		}
		streams = append(streams, g...)
	}
	if len(streams) == 0 {
		if xMediaRe.MatchString(raw) {
			return nil, fmt.Errorf("该推文中没有视频（只有文字或图片）")
		}
		return nil, fmt.Errorf("推文中没有找到视频（敏感内容可能需登录后可见）")
	}
	return dedupe(streams), nil
}

// xMediaRe matches tweet image URLs (as opposed to profile images), used to
// tell image-only tweets apart from login-walled or empty pages.
var xMediaRe = regexp.MustCompile(`https://pbs\.twimg\.com/media/`)

// xStreamMeta classifies an x.com video URL into its group key (video id,
// "" when standalone), stream kind and codec label. Video URLs look like
// https://video.twimg.com/amplify_video/<id>/vid/<codec>/<WxH>/<file>.mp4
// https://video.twimg.com/amplify_video/<id>/pl/<file>.m3u8
// https://video.twimg.com/tweet_video/<file>.mp4  (GIF rendering)
func xStreamMeta(u string) (key, kind, label string) {
	if strings.Contains(u, ".m3u8") {
		kind = "hls"
	} else {
		kind = "direct"
	}
	label = "h264"
	pu, err := url.Parse(u)
	if err != nil {
		return "", kind, label
	}
	segs := strings.Split(pu.Path, "/")
	if len(segs) >= 3 && (segs[1] == "amplify_video" || segs[1] == "ext_tw_video") {
		key = segs[2]
	}
	for i, s := range segs {
		if s == "vid" && i+1 < len(segs) && strings.Contains(strings.ToLower(segs[i+1]), "hvc") {
			label = "h265"
		}
	}
	return key, kind, label
}

// xRank orders variants within a group: direct mp4 by pixel count desc, HLS
// last.
func xRank(s Stream) int {
	if s.Kind == "hls" {
		return -1
	}
	if i := strings.Index(s.URL, "/vid/"); i > 0 {
		if parts := strings.SplitN(s.URL[i+5:], "/", 3); len(parts) >= 2 {
			if w, h, ok := parseWxH(parts[1]); ok {
				return w * h
			}
		}
	}
	return 0
}

func parseWxH(s string) (w, h int, ok bool) {
	x := strings.IndexByte(s, 'x')
	if x <= 0 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(s[:x])
	h, errH := strconv.Atoi(s[x+1:])
	return w, h, errW == nil && errH == nil
}
