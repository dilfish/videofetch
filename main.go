package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

func main() {
	var (
		pageURL = flag.String("url", "", "page URL to fetch")
		pick    = flag.String("pick", "", "download only the listed streams, omit to download all (1-based, matches -list row numbers; e.g. 1,3 or 1-4)")
		proxy   = flag.String("proxy", "", "proxy: http://host:port or socks5://host:port (socks5h resolves DNS remotely)")
		referer = flag.String("referer", "", "Referer header (default: the page's own origin)")
		ua      = flag.String("ua", defaultUA, "User-Agent header")
		list    = flag.Bool("list", false, "list discovered streams and exit")
		workers = flag.Int("workers", 8, "HLS segment download concurrency")
		quality = flag.String("quality", "best", "stream preference: best / lowest (master playlist bandwidth), h264 / h265 (dual-encoding players)")
		images  = flag.Bool("images", false, "also extract image attachments (<img> off-host urls); shown as kind image rows and downloaded as-is")
		timeout = flag.Int("timeout", 60, "HTTP response-header timeout in seconds")
		ex      = flag.Bool("ex", false, "when no stream is found, retry via the site's unlock flow (currently lurl.cc password gate; combine with -pass)")
		pass    = flag.String("pass", "", "password for -ex unlock flows")
		serve   = flag.Bool("serve", false, "run the video management HTTP server instead of downloading")
		addr    = flag.String("addr", ":8080", "server listen address; defaults to all interfaces so LAN devices can reach it (use 127.0.0.1:8080 to restrict to this machine)")
		dir     = flag.String("dir", ".", "directory of videos to serve")
		version = flag.Bool("v", false, "print Go build info and exit")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, helpText)
	}
	flag.Parse()
	if *version {
		printBuildInfo()
		return
	}
	if *serve {
		if err := runServer(*addr, *dir); err != nil {
			fatal(err)
		}
		return
	}
	if *pageURL == "" && flag.NArg() > 0 {
		*pageURL = flag.Arg(0)
	}
	if *pageURL == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *workers < 1 {
		*workers = 1
	}

	ref := *referer
	if ref == "" {
		if u, err := url.Parse(*pageURL); err == nil && u.Scheme != "" {
			ref = u.Scheme + "://" + u.Host + "/"
		}
	}
	c, err := NewClient(*proxy, ref, *ua, time.Duration(*timeout)*time.Second)
	if err != nil {
		fatal(err)
	}

	streams, err := c.extractStreams(*pageURL, 0, map[string]bool{})
	if err != nil {
		fatal(err)
	}
	if len(streams) == 0 && *ex {
		if isDrropURL(*pageURL) {
			// drrop.cc pages carry no video URL in the HTML at all: the
			// unlock is a JSON API call that returns a signed stream URL.
			if *pass == "" {
				fatal(fmt.Errorf("drrop page needs a password: pass it with -pass"))
			}
			streams, err = extractDrrop(c, *pageURL, *pass)
			if err != nil {
				fatal(err)
			}
		} else {
			gated, err := c.unlockLurl(*pageURL, *pass)
			if err != nil {
				fatal(err)
			}
			if gated {
				// Re-extract with a fresh visited set: the unlocked page is
				// fetched through the session cookie the unlock left behind.
				streams, err = c.extractStreams(*pageURL, 0, map[string]bool{})
				if err != nil {
					fatal(err)
				}
				if len(streams) == 0 {
					// lurl.cc returns {"type":"SUCCESS"} even for a wrong
					// password; the real check is whether the re-fetched page
					// now exposes media.
					fatal(fmt.Errorf("lurl.cc unlock succeeded but %s still exposes no media (wrong password?)", *pageURL))
				}
			}
		}
	}
	if *images {
		imgs, ierr := c.extractImages(*pageURL)
		if ierr == nil {
			streams = append(streams, imgs...)
		}
	}
	if len(streams) == 0 {
		fatal(fmt.Errorf("no video streams found in %s", *pageURL))
	}
	shown := listStreams(streams)
	if *list {
		fmt.Printf("%-4s %-6s %-12s %-16s %s\n", "#", "kind", "label", "info", "url")
		for i, s := range shown {
			fmt.Printf("[%d] %-6s %-12s %-16s %s\n", i+1, s.Kind, s.Label, streamInfo(c, s, *quality), s.URL)
		}
		if n := len(streams) - len(shown); n > 0 {
			fmt.Printf("(%d stream(s) hidden: duplicate or lower-quality variants of the same video)\n", n)
		}
		return
	}

	// Default: download everything -list shows (one row per player, h265
	// preferred). -pick narrows the selection to specific rows.
	var picks []int
	var targets []Stream
	if *pick != "" {
		picks, err = parsePicks(*pick, len(shown))
		if err != nil {
			fatal(err)
		}
		for _, i := range picks {
			targets = append(targets, resolveVariant(streams, shown[i-1], *quality))
		}
	} else {
		for i, s := range shown {
			picks = append(picks, i+1)
			targets = append(targets, resolveVariant(streams, s, *quality))
		}
	}

	prefix := randomName()
	for n, s := range targets {
		out := outName(prefix, picks[n], s)
		fmt.Printf("picked [%s] %s: %s\n", s.Kind, s.Label, s.URL)
		if err := download(c, s, out, *workers, *quality); err != nil {
			if s.Kind == "image" {
				// one broken attachment should not kill a batch of images
				fmt.Fprintf(os.Stderr, "videofetch: skip %s: %v\n", s.URL, err)
				continue
			}
			fatal(err)
		}
		fmt.Printf("saved to %s\n", out)
	}
}

// parsePicks parses a 1-based pick specification such as "1,3", "1-4" or
// "2,5-6" into a sorted, de-duplicated list of indexes within [1, n].
func parsePicks(spec string, n int) ([]int, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("empty -pick")
	}
	seen := map[int]bool{}
	var out []int
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, fmt.Errorf("invalid -pick %q: empty item", spec)
		}
		if lo, hi, ok := strings.Cut(tok, "-"); ok {
			a, errA := strconv.Atoi(strings.TrimSpace(lo))
			b, errB := strconv.Atoi(strings.TrimSpace(hi))
			if errA != nil || errB != nil || a < 1 || b < a {
				return nil, fmt.Errorf("invalid -pick %q: bad range %q (indexes are 1-based, a <= b)", spec, tok)
			}
			for x := a; x <= b; x++ {
				if err := addPick(&out, seen, x, spec, n); err != nil {
					return nil, err
				}
			}
			continue
		}
		x, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("invalid -pick %q: %q is not a number (indexes are 1-based)", spec, tok)
		}
		if err := addPick(&out, seen, x, spec, n); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("invalid -pick %q: no valid indexes", spec)
	}
	sort.Ints(out)
	return out, nil
}

func addPick(out *[]int, seen map[int]bool, x int, spec string, n int) error {
	if x < 1 || x > n {
		return fmt.Errorf("invalid -pick %q: index %d out of range (valid: 1..%d)", spec, x, n)
	}
	if !seen[x] {
		seen[x] = true
		*out = append(*out, x)
	}
	return nil
}

// resolveVariant maps a picked stream (as shown by -list) to the requested
// codec variant within the same player, falling back to the shown stream.
func resolveVariant(streams []Stream, s Stream, quality string) Stream {
	if quality != "h264" && quality != "h265" {
		return s
	}
	if s.Label == quality {
		return s
	}
	for _, x := range streams {
		if x.Group == s.Group && x.Label == quality {
			return x
		}
	}
	return s
}

// randomName returns a fresh 5-letter lowercase prefix whose indexed file
// names (e.g. "abcde.1.mp4") do not collide with existing files in the
// current directory.
func randomName() string {
	for {
		b := make([]byte, 5)
		for i := range b {
			b[i] = 'a' + byte(rand.Intn(26))
		}
		p := string(b)
		if matches, _ := filepath.Glob(p + ".*"); len(matches) == 0 {
			return p
		}
	}
}

// outName builds the output file name "<prefix>.<row>.<ext>", where row is
// the 1-based -list row number of the stream and ext comes from the stream
// URL (images keep their own extension, ".jpeg" is normalised to ".jpg").
func outName(prefix string, row int, s Stream) string {
	ext := urlExt(s.URL)
	switch {
	case ext == "":
		ext = ".mp4"
	case ext == ".jpeg":
		ext = ".jpg" // both spell out the same format
	}
	return fmt.Sprintf("%s.%d%s", prefix, row, ext)
}

// listStreams returns the streams to show in -list: one entry per player
// (preferring its h265 variant over h264), plus all standalone streams.
func listStreams(streams []Stream) []Stream {
	type grp struct{ h264, h265 *Stream }
	groups := map[string]*grp{}
	var order []string
	for i := range streams {
		s := &streams[i]
		if s.Group == "" {
			continue
		}
		g, ok := groups[s.Group]
		if !ok {
			g = &grp{}
			groups[s.Group] = g
			order = append(order, s.Group)
		}
		if s.Label == "h265" && g.h265 == nil {
			g.h265 = s
		}
		if s.Label == "h264" && g.h264 == nil {
			g.h264 = s
		}
	}
	out := make([]Stream, 0, len(order))
	for _, gid := range order {
		g := groups[gid]
		switch {
		case g.h265 != nil:
			out = append(out, *g.h265)
		case g.h264 != nil:
			out = append(out, *g.h264)
		}
	}
	for _, s := range streams {
		if s.Group == "" {
			out = append(out, s)
		}
	}
	return out
}

// streamInfo fetches lightweight metadata for the -list output: HLS streams
// report duration / segment count / resolution (from the playlist, no media
// downloaded), direct streams report the file size. "-" when unavailable.
func streamInfo(c *Client, s Stream, quality string) string {
	if s.Kind == "hls" {
		info, err := fetchHLSInfo(c, s.URL, quality)
		if err != nil {
			return "-"
		}
		dur := "-"
		if info.Duration >= 0 {
			dur = fmt.Sprintf("%d:%02d", int(info.Duration)/60, int(info.Duration)%60)
		}
		seg := ""
		if info.Segments > 0 {
			seg = fmt.Sprintf(" %d seg", info.Segments)
		}
		res := ""
		if info.Resolution != "" {
			res = " " + info.Resolution
		}
		return dur + seg + res
	}
	if size := directSize(c, s.URL); size >= 0 {
		return fmt.Sprintf("%.1f MB", float64(size)/1e6)
	}
	return "-"
}

func download(c *Client, s Stream, out string, workers int, quality string) error {
	switch s.Kind {
	case "hls":
		return DownloadHLS(c, s.URL, quality, out, workers)
	case "direct", "image":
		return DownloadDirect(c, s.URL, out)
	}
	return fmt.Errorf("unknown stream kind %q", s.Kind)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "videofetch: %v\n", err)
	os.Exit(1)
}

func printBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Fprintln(os.Stderr, "videofetch: build info unavailable")
		os.Exit(1)
	}
	fmt.Print(info)
}
