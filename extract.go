package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Stream is a video stream discovered on a page.
type Stream struct {
	URL   string // absolute URL
	Kind  string // "hls" (m3u8) or "direct" (mp4/webm/...)
	Label string // where it came from, e.g. "h264", "h265", "og:video"
	Group string // player instance the stream came from ("" = standalone)
}

var mediaExtRe = regexp.MustCompile(`(?i)\.(mp4|m3u8|flv|webm|mov|avi|ts)(\?|#|$)`)

func isMediaURL(raw string) bool {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "//")
	return mediaExtRe.MatchString(s)
}

func kindOf(u string) string {
	if strings.Contains(strings.ToLower(u), ".m3u8") {
		return "hls"
	}
	return "direct"
}

func resolveURL(base *url.URL, raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	ref := base.ResolveReference(u)
	ref.Fragment = ""
	return ref.String()
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && strings.EqualFold(ua.Host, ub.Host)
}

func dedupe(streams []Stream) []Stream {
	byURL := map[string]Stream{}
	order := []string{}
	for _, s := range streams {
		prev, ok := byURL[s.URL]
		if !ok {
			byURL[s.URL] = s
			order = append(order, s.URL)
			continue
		}
		// Prefer codec-specific labels (h264/h265) over generic ones so
		// that -quality can still select this stream.
		if codecLabel(s.Label) && !codecLabel(prev.Label) {
			byURL[s.URL] = s
		}
	}
	out := make([]Stream, 0, len(order))
	for _, u := range order {
		out = append(out, byURL[u])
	}
	return out
}

func codecLabel(l string) bool {
	return l == "h264" || l == "h265"
}

// extractStreams finds video streams on pageURL. iframes pointing at the same
// host are followed up to depth 2, because players often live on a sub-page.
func (c *Client) extractStreams(pageURL string, depth int, visited map[string]bool) ([]Stream, error) {
	if visited[pageURL] || depth > 2 {
		return nil, nil
	}
	visited[pageURL] = true

	body, err := c.GetBytes(pageURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}

	var streams []Stream
	add := func(raw, label string) {
		if !isMediaURL(raw) {
			return
		}
		if abs := resolveURL(base, raw); abs != "" {
			streams = append(streams, Stream{URL: abs, Kind: kindOf(abs), Label: label})
		}
	}

	// 1. DOM walk: <video>/<source>, og:video meta, JSON-LD, iframes.
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err == nil {
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				switch n.Data {
				case "video", "source":
					if src := attr(n, "src"); src != "" {
						add(src, "media tag")
					}
				case "meta":
					p := attr(n, "property")
					if p == "og:video" || p == "og:video:url" || p == "og:video:secure_url" ||
						attr(n, "name") == "twitter:player:stream" {
						add(attr(n, "content"), "og:video")
					}
				case "iframe":
					if src := attr(n, "src"); src != "" {
						abs := resolveURL(base, src)
						if abs != "" && sameHost(abs, pageURL) {
							sub, err := c.extractStreams(abs, depth+1, visited)
							if err == nil {
								streams = append(streams, sub...)
							}
							// a broken player page is skipped silently
						}
					}
				case "script":
					if strings.Contains(attr(n, "type"), "ld+json") && n.FirstChild != nil {
						collectJSONMedia(n.FirstChild.Data, "jsonld", base, add)
					}
				}
			}
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				walk(ch)
			}
		}
		walk(doc)
	}

	// 2. DPlayer / ArtPlayer style configs: data-config="...". Each config is
	// one player, so its streams share a group id — used by -list to hide the
	// h264 duplicate when an h265 variant exists.
	rest := string(body)
	for idx := 0; ; idx++ {
		v, ok := attrValue(rest, "data-config")
		if !ok {
			break
		}
		group := fmt.Sprintf("player%d", idx)
		collectJSONMedia(v, "dplayer", base, func(raw, label string) {
			if !isMediaURL(raw) {
				return
			}
			if abs := resolveURL(base, raw); abs != "" {
				streams = append(streams, Stream{URL: abs, Kind: kindOf(abs), Label: label, Group: group})
			}
		})
		if i := strings.Index(rest, "data-config"); i >= 0 {
			rest = rest[i+len("data-config"):]
		}
	}

	// 3. Any URL with a media extension anywhere in the raw HTML/JS.
	for _, u := range rawURLRe.FindAllString(html.UnescapeString(string(body)), -1) {
		add(u, "raw")
	}

	return dedupe(streams), nil
}

var rawURLRe = regexp.MustCompile(`(?:https?:)?//[^\s"'<>()\\]+`)

// attrValue returns the value of the first occurrence of `attr="..."` or
// `attr='...'` in s, handling backslash-escaped quotes and HTML entities.
func attrValue(s, attr string) (string, bool) {
	re := regexp.MustCompile(regexp.QuoteMeta(attr) + `\s*=`)
	loc := re.FindStringIndex(s)
	if loc == nil {
		return "", false
	}
	rest := s[loc[1]:]
	if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
		return "", false
	}
	q := rest[0]
	var b strings.Builder
	for i := 1; i < len(rest); i++ {
		ch := rest[i]
		if ch == '\\' && i+1 < len(rest) && rest[i+1] == q {
			b.WriteByte(q)
			i++
			continue
		}
		if ch == q {
			return html.UnescapeString(b.String()), true
		}
		b.WriteByte(ch)
	}
	return "", false
}

// collectJSONMedia walks decoded JSON and adds any string that looks like a
// media URL. DPlayer's two streams are labelled h264 (url/video) and h265
// (video_h265); JSON-LD stays labelled jsonld.
func collectJSONMedia(data, rootLabel string, base *url.URL, add func(string, string)) {
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return
	}
	var walk func(interface{}, string)
	walk = func(v interface{}, lbl string) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, val := range t {
				nl := lbl
				switch k {
				case "video_h265":
					nl = "h265"
				case "contentUrl":
					nl = "jsonld"
				case "url":
					if nl != "jsonld" {
						nl = "h264"
					}
				case "video":
					if _, ok := val.(string); ok {
						nl = "h264"
					}
				}
				walk(val, nl)
			}
		case []interface{}:
			for _, e := range t {
				walk(e, lbl)
			}
		case string:
			if isMediaURL(t) {
				add(t, lbl)
			}
		}
	}
	walk(v, rootLabel)
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
