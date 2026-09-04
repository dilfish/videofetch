package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- HLS playlist parsing ---

type variant struct {
	URL        string
	Bandwidth  int
	Resolution string
}

type hlsKey struct {
	uri string
	iv  []byte // nil => derive from media sequence number
}

type mediaSeg struct {
	url      string
	seq      int64
	duration float64
	init     string // EXT-X-MAP init section URL ("" = MPEG-TS stream)
}

// parseAttrList parses "NAME=value,NAME2=\"v,with comma\"" attribute lists,
// stripping a leading tag name up to the first ':'.
func parseAttrList(line string) map[string]string {
	s := line
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	out := map[string]string{}
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		j := i
		for j < len(s) && s[j] != '=' && s[j] != ',' {
			j++
		}
		if j >= len(s) || s[j] != '=' {
			break
		}
		key := strings.TrimSpace(s[i:j])
		i = j + 1
		if i < len(s) && s[i] == '"' {
			i++
			var b strings.Builder
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote
			out[key] = b.String()
		} else {
			j = i
			for j < len(s) && s[j] != ',' {
				j++
			}
			out[key] = strings.TrimSpace(s[i:j])
			i = j
		}
	}
	return out
}

func parseIV(s string) []byte {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return nil // fall back to the sequence-number default
	}
	return b
}

func selectVariant(raw string, base *url.URL, quality string) (variant, error) {
	var best *variant
	var pendingBw int
	var pendingRes string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			attrs := parseAttrList(line)
			pendingBw, _ = strconv.Atoi(attrs["BANDWIDTH"])
			pendingRes = attrs["RESOLUTION"]
			continue
		}
		if pendingBw > 0 && line != "" && !strings.HasPrefix(line, "#") {
			v := variant{URL: resolveURL(base, line), Bandwidth: pendingBw, Resolution: pendingRes}
			if best == nil ||
				(quality == "best" && v.Bandwidth > best.Bandwidth) ||
				(quality != "best" && v.Bandwidth < best.Bandwidth) {
				best = &v
			}
			pendingBw = 0
			pendingRes = ""
		}
	}
	if best == nil {
		return variant{}, fmt.Errorf("no variant found in master playlist")
	}
	return *best, nil
}

func parseMediaPlaylist(raw string, base *url.URL) ([]mediaSeg, *hlsKey, int64, error) {
	var segs []mediaSeg
	var key *hlsKey
	var mediaSeq int64
	var initURL string // current EXT-X-MAP init section ("" = none)
	pending := false
	pendingDur := 0.0
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("bad media sequence %q", line)
			}
			mediaSeq = seq
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs := parseAttrList(line)
			switch attrs["METHOD"] {
			case "AES-128":
				key = &hlsKey{uri: attrs["URI"], iv: parseIV(attrs["IV"])}
			case "NONE":
				key = nil
			default:
				return nil, nil, 0, fmt.Errorf("unsupported encryption METHOD %q", attrs["METHOD"])
			}
		case strings.HasPrefix(line, "#EXT-X-MAP"):
			attrs := parseAttrList(line)
			if attrs["BYTERANGE"] != "" {
				return nil, nil, 0, fmt.Errorf("init section BYTERANGE is not supported")
			}
			if attrs["URI"] == "" {
				return nil, nil, 0, fmt.Errorf("EXT-X-MAP without URI")
			}
			initURL = resolveURL(base, attrs["URI"])
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			return nil, nil, 0, fmt.Errorf("BYTERANGE segments are not supported")
		case strings.HasPrefix(line, "#EXTINF"):
			durStr := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(durStr, ','); i >= 0 {
				durStr = durStr[:i]
			}
			pendingDur, _ = strconv.ParseFloat(durStr, 64)
			pending = true
		case strings.HasPrefix(line, "#"):
			// other tags are ignored
		case line != "":
			if !pending {
				return nil, nil, 0, fmt.Errorf("segment line without #EXTINF: %q", line)
			}
			segs = append(segs, mediaSeg{
				url:      resolveURL(base, line),
				seq:      mediaSeq + int64(len(segs)),
				duration: pendingDur,
				init:     initURL,
			})
			pending = false
			pendingDur = 0
		}
	}
	if len(segs) == 0 {
		return nil, nil, 0, fmt.Errorf("no media segments found in playlist")
	}
	return segs, key, mediaSeq, nil
}

// --- segment download + AES-128-CBC decryption ---

// deriveIV returns the per-segment IV: the EXT-X-KEY IV attribute plus the
// segment's offset from the media sequence, or the sequence number itself
// (big-endian) when no IV attribute is present.
func deriveIV(key *hlsKey, seq, mediaSeq int64) []byte {
	iv := make([]byte, 16)
	if key != nil && key.iv != nil {
		copy(iv, key.iv)
		for inc := seq - mediaSeq; inc > 0; inc-- {
			for j := 15; j >= 0; j-- {
				iv[j]++
				if iv[j] != 0 {
					break
				}
			}
		}
		return iv
	}
	for j := 15; j >= 0; j-- {
		iv[j] = byte(seq)
		seq >>= 8
	}
	return iv
}

func aesDecrypt(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	dec := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(dec, data)
	// Strip PKCS7 padding, but only when it looks valid.
	if n := int(dec[len(dec)-1]); n >= 1 && n <= aes.BlockSize {
		ok := true
		for _, b := range dec[len(dec)-n:] {
			if b != byte(n) {
				ok = false
				break
			}
		}
		if ok {
			return dec[:len(dec)-n], nil
		}
	}
	return dec, nil
}

func fetchSegment(c *Client, u string, keyData, iv []byte) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		data, err := c.GetBytes(u)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			continue
		}
		if keyData == nil {
			return data, nil
		}
		return aesDecrypt(data, keyData, iv)
	}
	return nil, fmt.Errorf("segment %s: %v", u, lastErr)
}

// fetchPlaylist resolves a playlist URL (following a master playlist when
// present) and returns the fully downloaded, decrypted, concatenated stream.
func fetchPlaylist(c *Client, playlistURL, quality string, workers int) ([]byte, error) {
	raw, err := c.GetBytes(playlistURL)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(raw), "#EXT-X-STREAM-INF") {
		v, err := selectVariant(string(raw), base, quality)
		if err != nil {
			return nil, err
		}
		fmt.Printf("master playlist: selected variant %s (%d bps, %s)\n", v.URL, v.Bandwidth, v.Resolution)
		return fetchPlaylist(c, v.URL, quality, workers)
	}

	segs, key, mediaSeq, err := parseMediaPlaylist(string(raw), base)
	if err != nil {
		return nil, err
	}
	fmt.Printf("media playlist: %d segments\n", len(segs))

	var keyData []byte
	if key != nil {
		keyData, err = c.GetBytes(resolveURL(base, key.uri))
		if err != nil {
			return nil, fmt.Errorf("fetch AES key: %w", err)
		}
		if len(keyData) != 16 {
			return nil, fmt.Errorf("AES key length %d != 16", len(keyData))
		}
	}

	// fMP4: fetch the init section (EXT-X-MAP) and prepend it to the output.
	// It is encrypted like the first segment when AES-128 is in effect.
	var prefix []byte
	if initURL := segs[0].init; initURL != "" {
		for _, s := range segs {
			if s.init != initURL {
				return nil, fmt.Errorf("multiple EXT-X-MAP init sections are not supported")
			}
		}
		initData, err := c.GetBytes(initURL)
		if err != nil {
			return nil, fmt.Errorf("fetch init section: %w", err)
		}
		if key != nil {
			initData, err = aesDecrypt(initData, keyData, deriveIV(key, mediaSeq, mediaSeq))
			if err != nil {
				return nil, fmt.Errorf("decrypt init section: %w", err)
			}
		}
		prefix = initData
	}

	out := make([][]byte, len(segs))
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for i, seg := range segs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, seg mediaSeg) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := fetchSegment(c, seg.url, keyData, deriveIV(key, seg.seq, mediaSeq))
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			out[i] = data
			if (i+1)%10 == 0 || i+1 == len(segs) {
				fmt.Printf("\rsegments %d/%d", i+1, len(segs))
			}
		}(i, seg)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	fmt.Printf("\rsegments %d/%d done\n", len(segs), len(segs))
	return bytes.Join(append([][]byte{prefix}, out...), nil), nil
}

// HLSInfo is the metadata shown by -list for an HLS stream.
type HLSInfo struct {
	Duration   float64 // seconds; -1 when unknown
	Segments   int
	Resolution string // from the selected master variant, "" for plain playlists
	Bandwidth  int
}

// fetchHLSInfo resolves a playlist (following a master playlist with the
// given quality preference) and reports duration / segment count without
// downloading any media segments.
func fetchHLSInfo(c *Client, playlistURL, quality string) (HLSInfo, error) {
	info := HLSInfo{Duration: -1}
	raw, err := c.GetBytes(playlistURL)
	if err != nil {
		return info, err
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		return info, err
	}
	if strings.Contains(string(raw), "#EXT-X-STREAM-INF") {
		v, err := selectVariant(string(raw), base, quality)
		if err != nil {
			return info, err
		}
		info.Resolution = v.Resolution
		info.Bandwidth = v.Bandwidth
		raw, err = c.GetBytes(v.URL)
		if err != nil {
			return info, err
		}
		base, err = url.Parse(v.URL)
		if err != nil {
			return info, err
		}
	}
	segs, _, _, err := parseMediaPlaylist(string(raw), base)
	if err != nil {
		return info, err
	}
	info.Segments = len(segs)
	info.Duration = 0
	for _, s := range segs {
		info.Duration += s.duration
	}
	return info, nil
}

// DownloadHLS downloads an HLS stream to outPath. Raw TS segments are
// concatenated first; ffmpeg remuxes the result to the target container
// when available (and the target name is not .ts).
func DownloadHLS(c *Client, playlistURL, quality, outPath string, workers int) error {
	data, err := fetchPlaylist(c, playlistURL, quality, workers)
	if err != nil {
		return err
	}
	tsPath := outPath + ".part"
	if err := os.WriteFile(tsPath, data, 0o644); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(outPath), ".ts") {
		return os.Rename(tsPath, outPath)
	}
	if !haveFFmpeg() {
		if err := os.Rename(tsPath, outPath); err != nil {
			return err
		}
		fmt.Printf("ffmpeg not found: output is the raw container (MPEG-TS or fragmented MP4) despite the %q name; remux later with: ffmpeg -i %q -c copy out.mp4\n", outPath, outPath)
		return nil
	}
	if err := remux(tsPath, outPath); err != nil {
		return fmt.Errorf("ffmpeg remux: %v (raw TS kept at %s)", err, tsPath)
	}
	return os.Remove(tsPath)
}

func haveFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func remux(src, dst string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-c", "copy", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
