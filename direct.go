package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DownloadDirect streams a plain file (mp4/webm/...) to outPath. When the
// server supports ranges, an interrupted download resumes from the offset.
func DownloadDirect(c *Client, u, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var offset, total int64 = 0, -1
	canResume := false
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		c.applyHeaders(req)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusPartialContent {
			canResume = true
		}
		if resp.StatusCode == http.StatusOK && offset > 0 {
			// Server ignored the Range header: restart from the beginning.
			offset = 0
			f.Truncate(0)
			f.Seek(0, io.SeekStart)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		if total < 0 && resp.ContentLength > 0 {
			total = resp.ContentLength + offset
			fmt.Printf("downloading %s (%.1f MB)\n", u, float64(total)/1e6)
		}
		n, err := copyProgress(f, resp.Body, offset, total)
		resp.Body.Close()
		offset += n
		if err == nil {
			fmt.Println()
			return nil
		}
		lastErr = err
		if !canResume {
			return fmt.Errorf("download interrupted and the server does not support resume: %w", err)
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return fmt.Errorf("download failed after retries: %v", lastErr)
}

// directSize returns the file size in bytes via HEAD, or -1 when unknown.
func directSize(c *Client, u string) int64 {
	resp, err := c.Head(u)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	return resp.ContentLength
}

func copyProgress(dst io.Writer, src io.Reader, offset, total int64) (int64, error) {
	buf := make([]byte, 256*1024)
	var n int64
	for {
		m, err := src.Read(buf)
		if m > 0 {
			dst.Write(buf[:m])
			n += int64(m)
			done := offset + n
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				fmt.Printf("\r%.1f%% (%d/%d MB)", pct, done/1e6, total/1e6)
			} else {
				fmt.Printf("\r%d MB", done/1e6)
			}
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}
