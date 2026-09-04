package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// Client is an HTTP client with proxy / Referer / UA support.
// All requests made through it (page, playlists, keys, segments) carry the
// same headers, which is what most anti-hotlink CDNs check.
type Client struct {
	http    *http.Client
	referer string
	ua      string
}

func NewClient(proxyURL, referer, ua string, headerTimeout time.Duration) (*Client, error) {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
		}
		switch u.Scheme {
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			// x/net/proxy sends the hostname to the proxy as-is, so DNS is
			// resolved on the proxy side (socks5h behavior) — local DNS
			// pollution (GFW) cannot poison the lookup.
			var auth *proxy.Auth
			if u.User != nil {
				pw, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: pw}
			}
			d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("socks5 dialer %q: %w", u.Host, err)
			}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q (use http://, socks5:// or socks5h://)", u.Scheme)
		}
	}
	return &Client{
		http:    &http.Client{Transport: tr}, // no overall timeout: downloads can be long
		referer: referer,
		ua:      ua,
	}, nil
}

// GetBytes performs a GET and returns the full body, failing on non-200.
func (c *Client) GetBytes(u string) ([]byte, error) {
	resp, err := c.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Get performs a GET with the client's default headers.
func (c *Client) Get(u string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	return c.http.Do(req)
}

// Head performs a HEAD request with the client's default headers.
func (c *Client) Head(u string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	return c.http.Do(req)
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", c.ua)
	if c.referer != "" {
		req.Header.Set("Referer", c.referer)
	}
}
