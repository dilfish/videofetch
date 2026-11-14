package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// imageExtRe matches image file extensions in URLs.
var imageExtRe = regexp.MustCompile(`(?i)\.(jpe?g|png|webp|gif|bmp)(\?|#|$)`)

func isImageURL(raw string) bool {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "//")
	return imageExtRe.MatchString(s)
}

// imgAttrs lists the <img> attributes scanned for the image URL, lazy-load
// attributes first. Forum themes put ad-block baits or empty placeholders in
// src / iyl-data while the real attachment hides in data-src / ess-data /
// file, so a placeholder must never win over a later real attribute.
var imgAttrs = []string{"data-src", "ess-data", "file", "iyl-data", "src"}

// extractImages collects image attachments from a page: <img> tags whose URL
// carries an image extension. Same-host images (site logos, avatars, smileys)
// are skipped; off-host attachments are kept. Order is document order,
// de-duplicated by URL.
func (c *Client) extractImages(pageURL string) ([]Stream, error) {
	body, err := c.GetBytes(pageURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	byURL := map[string]bool{}
	var out []Stream
	add := func(raw string) {
		if raw == "" || !isImageURL(raw) {
			return
		}
		abs := resolveURL(base, raw)
		if abs == "" || byURL[abs] {
			return
		}
		if sameHost(abs, pageURL) {
			return
		}
		byURL[abs] = true
		out = append(out, Stream{URL: abs, Kind: "image", Label: "img"})
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, k := range imgAttrs {
				v := attr(n, k)
				if v == "" || strings.HasPrefix(strings.ToLower(v), "data:") {
					continue
				}
				add(v)
				break // first attribute that yields an image URL wins
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	return out, nil
}

// urlExt returns the file extension of a URL path (".jpg" style, lowercase
// preserved from the URL), or "" when the path has none.
func urlExt(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(pu.Path))
	return ext
}
