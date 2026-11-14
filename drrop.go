package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// drrop.cc is a Next.js SPA: the page HTML carries no video URL and no form.
// Unlocking happens through its JSON API, which is fully usable without a
// browser:
//
//	POST https://api.drrop.cc/api/verify-password
//	     {"code": "<id>", "password": "<pw>"}
//	     → {"token": "stream:<id>.<ts>.<hex>", "expires_in": 300, ...}
//	GET  https://api.drrop.cc/api/stream/<code>?t=<token>   (video/mp4)
//
// The token is not bound to any cookie, supports Range, and expires after
// 5 minutes — plenty for a direct download.

const drropAPIBase = "https://api.drrop.cc"

// isDrropURL reports whether u is a video page on drrop.cc (/v/<code>).
func isDrropURL(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	if strings.ToLower(pu.Host) != "drrop.cc" && strings.ToLower(pu.Host) != "www.drrop.cc" {
		return false
	}
	return drropCode(pu.Path) != ""
}

// drropCode extracts the numeric code from a /v/<code> path ("" when absent).
func drropCode(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) >= 2 && segs[0] == "v" && segs[1] != "" {
		return segs[1]
	}
	return ""
}

// drropVerifyResponse is the reply of /api/verify-password. engage_token is
// for likes/comments and not needed for streaming.
type drropVerifyResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	EngageTok string `json:"engage_token"`
	Error     string `json:"error"`
}

// extractDrrop unlocks a drrop.cc video page with the password and returns
// its signed direct stream.
func extractDrrop(c *Client, pageURL, password string) ([]Stream, error) {
	pu, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	code := drropCode(pu.Path)
	if code == "" {
		return nil, fmt.Errorf("not a drrop video page: %s", pageURL)
	}
	if password == "" {
		return nil, fmt.Errorf("drrop page needs a password: pass it with -password")
	}
	payload, _ := json.Marshal(map[string]string{"code": code, "password": password})
	var resp drropVerifyResponse
	if err := c.PostJSON(drropAPIBase+"/api/verify-password", payload, &resp); err != nil {
		return nil, fmt.Errorf("drrop verify-password: %w", err)
	}
	if resp.Error != "" || resp.Token == "" {
		switch resp.Error {
		case "wrong_password":
			return nil, fmt.Errorf("drrop: wrong password")
		case "expired":
			return nil, fmt.Errorf("drrop: password attempt expired, retry")
		case "limit_reached":
			return nil, fmt.Errorf("drrop: attempt limit reached")
		case "disabled":
			return nil, fmt.Errorf("drrop: link disabled")
		case "":
			return nil, fmt.Errorf("drrop: no token in response")
		default:
			return nil, fmt.Errorf("drrop: %s", resp.Error)
		}
	}
	stream := fmt.Sprintf("%s/api/stream/%s?t=%s", drropAPIBase, url.PathEscape(code), url.QueryEscape(resp.Token))
	return []Stream{{URL: stream, Kind: "direct", Label: "drrop"}}, nil
}
