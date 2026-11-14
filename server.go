package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed index.html
var webFS embed.FS

// videoExts are the file extensions served and listed as videos.
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".m4v": true, ".webm": true,
	".mkv": true, ".ts": true, ".flv": true, ".avi": true,
}

type videoInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	URL     string    `json:"url"`
}

// runServer starts the video management HTTP server. addr is the listen
// address, dir the directory scanned for videos. It blocks until the server
// exits.
func runServer(addr, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dir); err != nil {
		return fmt.Errorf("video directory: %w", err)
	} else if !st.IsDir() {
		return fmt.Errorf("video directory %s is not a directory", dir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/videos", handleVideos(dir))
	mux.HandleFunc("/api/videos/", handleDeleteVideo(dir))
	mux.HandleFunc("/videos/", handleStream(dir))

	printURLs(addr, dir)
	return http.ListenAndServe(addr, mux)
}

// printURLs logs how to reach the server: the local URL, every LAN URL, and
// a hint when the listener is bound to loopback only.
func printURLs(addr, dir string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		log.Printf("videofetch server: http://%s  (videos: %s)", addr, dir)
		return
	}
	localHost := host
	if localHost == "" || localHost == "0.0.0.0" || localHost == "::" {
		localHost = "127.0.0.1"
	}
	log.Printf("videofetch server: http://%s:%s  (videos: %s)", localHost, port, dir)

	lan := lanAddrs()
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		if len(lan) > 0 {
			log.Printf("提示: 当前只监听了 %s，局域网设备无法访问；请用 -addr :%s 重启（例: http://%s:%s）", host, port, lan[0], port)
		} else {
			log.Printf("提示: 当前只监听了 %s，局域网设备无法访问；请用 -addr :%s 重启", host, port)
		}
		return
	}
	// Listening on a wildcard or a specific interface: print the LAN URLs
	// other devices can use, best candidate first. A specific bind only
	// serves that address, so list just it.
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		log.Printf("局域网访问: http://%s:%s", formatHost(host), port)
		return
	}
	if len(lan) == 0 {
		log.Printf("提示: 未发现局域网地址，请确认已连接网络")
		return
	}
	for _, ip := range lan {
		log.Printf("局域网访问: http://%s:%s", ip, port)
	}
	log.Printf("提示: 同网段设备均可打开以上地址，并可查看/删除 %s 下的视频", dir)
}

// formatHost wraps an IPv6 literal in brackets so it can be used in a URL.
func formatHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// lanAddrs returns the machine's non-loopback IP addresses on up interfaces,
// ordered so the ones other LAN devices are most likely to reach come first:
// private IPv4 (192.168/10/172.16), then other IPv4, then IPv6. IPv6 literals
// are returned already bracketed for use in URLs.
func lanAddrs() []string {
	var private4, public4, v6 []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				if ip4.IsPrivate() {
					private4 = append(private4, ip4.String())
				} else {
					public4 = append(public4, ip4.String())
				}
				continue
			}
			v6 = append(v6, formatHost(ip.String()))
		}
	}
	out := make([]string, 0, len(private4)+len(public4)+len(v6))
	out = append(out, private4...)
	out = append(out, public4...)
	out = append(out, v6...)
	return out
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	html, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(html)
}

// handleVideos lists the video files in dir as JSON.
func handleVideos(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		videos := []videoInfo{}
		for _, e := range entries {
			if e.IsDir() || !videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			videos = append(videos, videoInfo{
				Name:    e.Name(),
				Size:    info.Size(),
				ModTime: info.ModTime(),
				URL:     "/videos/" + e.Name(),
			})
		}
		sort.Slice(videos, func(i, j int) bool { return videos[i].Name < videos[j].Name })
		writeJSON(w, map[string]any{"videos": videos})
	}
}

// handleDeleteVideo deletes a video file by name.
func handleDeleteVideo(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/videos/")
		if !safeName(name) {
			http.Error(w, "invalid video name", http.StatusBadRequest)
			return
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "video not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleStream streams a video file, honoring Range requests so the HTML5
// player can seek in large files.
func handleStream(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/videos/")
		if !safeName(name) {
			http.Error(w, "invalid video name", http.StatusBadRequest)
			return
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "video not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, name, st.ModTime(), f)
	}
}

// safeName rejects names that could escape the video directory: it must be a
// plain file name with no path separators or "..".
func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && name == filepath.Base(name)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
