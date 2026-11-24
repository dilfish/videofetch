package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// imageExtRe matches image file extensions in URLs.
var imageExtRe = regexp.MustCompile(`(?i)\.(jpe?g|png|webp|gif|bmp)(\?|#|$)`)

// canvasImgRe matches JavaScript calls that paint an attachment into a
// <canvas> instead of an <img>: lurl.cc photo pages write every picture as
// canvas_img('https://lurlN.lurl.cc/<date>/<hash>.jpeg','canvas_1','0').
// Only the first argument is the image URL; later ones name the canvas.
var canvasImgRe = regexp.MustCompile(`canvas_img\s*\(\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')`)

// jsString decodes a JS string literal captured from the page. The common
// case is a URL that was JSON-serialized, so "\/" and "\u0026" have to be
// undone; anything strconv cannot decode is left as-is (best effort, a
// malformed escape must not drop the whole attachment).
func jsString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	if v, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return v
	}
	return strings.ReplaceAll(s, `\/`, "/")
}

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
// carries an image extension, plus canvas_img(...) calls that draw the
// picture into a <canvas> instead (lurl.cc photo pages). Same-host <img>
// images (site logos, avatars, smileys) are skipped; off-host attachments are
// kept. Order is document order (all <img> rows, then canvas rows),
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
	add := func(raw, label string, skipSameHost bool) {
		if raw == "" || !isImageURL(raw) {
			return
		}
		abs := resolveURL(base, raw)
		if abs == "" || byURL[abs] {
			return
		}
		if skipSameHost && sameHost(abs, pageURL) {
			return
		}
		byURL[abs] = true
		out = append(out, Stream{URL: abs, Kind: "image", Label: label})
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, k := range imgAttrs {
				v := attr(n, k)
				if v == "" || strings.HasPrefix(strings.ToLower(v), "data:") {
					continue
				}
				add(v, "img", true)
				break // first attribute that yields an image URL wins
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)

	// Canvas-rendered attachments (lurl.cc photo pages): the URL only exists
	// as the first argument of a canvas_img(...) call, so the DOM walk sees
	// nothing. HTML comments are stripped first — the same decoy rule the
	// video extractor follows. These calls are an explicit "draw this
	// attachment" statement, so the same-host chrome filter does not apply.
	for _, m := range canvasImgRe.FindAllStringSubmatch(htmlCommentRe.ReplaceAllString(string(body), ""), -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		add(jsString(raw), "canvas", false)
	}
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
