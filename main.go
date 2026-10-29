package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/*.html
var webFS embed.FS

type Config struct {
	Port     int    `json:"port"`
	RecvDir  string `json:"recvDir"`
	ShareDir string `json:"shareDir"`
	ShareOn  bool   `json:"shareOn"`
	IP       string `json:"ip"`
}

var (
	mu      sync.Mutex
	cfg     Config
	cfgPath string
	srv     *http.Server
	handler http.Handler
	curPort atomic.Int32
)

func get() Config { mu.Lock(); defer mu.Unlock(); return cfg }

func saveLocked() {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(cfgPath, b, 0644)
}

func loadConfig() {
	exe, _ := os.Executable()
	cfgPath = filepath.Join(filepath.Dir(exe), "shareUp.json")
	home, _ := os.UserHomeDir()
	cfg = Config{Port: 8080, RecvDir: filepath.Join(home, "Downloads", "shareUp")}
	if b, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		cfg.Port = 8080
	}
}

func start(port int) (int, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return 0, err
	}
	s := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go s.Serve(ln)
	old := srv
	srv = s
	if old != nil {
		go func() {
			ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			if old.Shutdown(ctx) != nil {
				old.Close()
			}
		}()
	}
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func localIPs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if v4 := n.IP.To4(); v4 != nil && v4.IsPrivate() {
					out = append(out, v4.String())
				}
			}
		}
	}
	return out
}

func pickIP(c Config, ips []string) string {
	for _, ip := range ips {
		if ip == c.IP {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

func isLocal(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, l := range localIPs() {
		if l == ip.String() {
			return true
		}
	}
	return false
}

func local(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocal(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/" + name)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}
}

func adminState(w http.ResponseWriter, r *http.Request) {
	c := get()
	ips := localIPs()
	ip := pickIP(c, ips)
	u := ""
	if ip != "" {
		u = fmt.Sprintf("http://%s:%d", ip, curPort.Load())
	}
	writeJSON(w, map[string]any{"port": curPort.Load(), "recvDir": c.RecvDir,
		"shareDir": c.ShareDir, "shareOn": c.ShareOn, "ip": ip, "ips": ips, "url": u})
}

func adminSet(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Port     *int    `json:"port"`
		RecvDir  *string `json:"recvDir"`
		ShareDir *string `json:"shareDir"`
		ShareOn  *bool   `json:"shareOn"`
		IP       *string `json:"ip"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if q.Port != nil && *q.Port != int(curPort.Load()) {
		if *q.Port < 1024 || *q.Port > 65535 {
			http.Error(w, "Port must be 1024-65535", 400)
			return
		}
		p, err := start(*q.Port)
		if err != nil {
			http.Error(w, "Port busy, try another", 400)
			return
		}
		curPort.Store(int32(p))
		mu.Lock()
		cfg.Port = p
		mu.Unlock()
	}
	mu.Lock()
	if q.RecvDir != nil {
		cfg.RecvDir = strings.TrimSpace(*q.RecvDir)
	}
	if q.ShareDir != nil {
		cfg.ShareDir = strings.TrimSpace(*q.ShareDir)
	}
	if q.ShareOn != nil {
		cfg.ShareOn = *q.ShareOn
	}
	if q.IP != nil {
		cfg.IP = *q.IP
	}
	saveLocked()
	mu.Unlock()
	adminState(w, r)
}

func adminBrowse(w http.ResponseWriter, r *http.Request) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
		case "windows":
		script := `[Console]::OutputEncoding=[Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$o = New-Object System.Windows.Forms.Form
$o.TopMost = $true
$o.ShowInTaskbar = $false
$o.FormBorderStyle = 'None'
$o.Opacity = 0
$o.StartPosition = 'CenterScreen'
$o.Size = New-Object System.Drawing.Size(1,1)
$o.Show()
[System.Windows.Forms.SendKeys]::SendWait('%')
$o.Activate()
$f = New-Object System.Windows.Forms.FolderBrowserDialog
if ($f.ShowDialog($o) -eq 'OK') { Write-Output $f.SelectedPath }
$o.Dispose()`
		cmd = exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	case "darwin":
		cmd = exec.Command("osascript", "-e", `POSIX path of (choose folder)`)
	default:
		cmd = exec.Command("zenity", "--file-selection", "--directory")
	}
	out, _ := cmd.Output()
	writeJSON(w, map[string]string{"path": strings.TrimSpace(string(out))})
}

func adminQR(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if !strings.HasPrefix(u, "http://") {
		http.Error(w, "bad", 400)
		return
	}
	png, err := qrcode.Encode(u, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "qr error", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

var bad = strings.NewReplacer("<", "_", ">", "_", ":", "_", "\"", "_", "/", "_", "\\", "_", "|", "_", "?", "_", "*", "_")

func cleanName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	n = n[strings.LastIndex(n, "/")+1:]
	n = strings.TrimRight(bad.Replace(n), ". ")
	if n == "" {
		n = "file"
	}
	return n
}

func upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	c := get()
	if c.RecvDir == "" {
		http.Error(w, "no folder", 500)
		return
	}
	if err := os.MkdirAll(c.RecvDir, 0755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	name := cleanName(r.URL.Query().Get("name"))
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	var f *os.File
	var dest string
	for i := 0; ; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		dest = filepath.Join(c.RecvDir, n)
		var err error
		f, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	_, err := io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		os.Remove(dest)
		http.Error(w, err.Error(), 500)
		return
	}
	fmt.Println("Received:", dest)
	writeJSON(w, map[string]any{"ok": true})
}

func list(w http.ResponseWriter, r *http.Request) {
	c := get()
	type item struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir"`
		Size int64  `json:"size"`
		T    int64  `json:"t"`
	}
	out := []item{}
	on := c.ShareOn && c.ShareDir != ""
	sub := ""
	if on {
		base := filepath.Clean(c.ShareDir)
		sub = strings.Trim(filepath.ToSlash(r.URL.Query().Get("path")), "/")
		full := filepath.Join(base, filepath.FromSlash(sub))
		rel, err := filepath.Rel(base, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			http.Error(w, "forbidden", 403)
			return
		}
		entries, _ := os.ReadDir(full)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, item{e.Name(), e.IsDir(), info.Size(), info.ModTime().Unix()})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Dir != out[j].Dir {
				return out[i].Dir
			}
			return out[i].T > out[j].T
		})
	}
	writeJSON(w, map[string]any{"shareOn": on, "path": sub, "items": out})
}

func download(w http.ResponseWriter, r *http.Request) {
	c := get()
	if !c.ShareOn || c.ShareDir == "" {
		http.Error(w, "sharing off", 403)
		return
	}
	base := filepath.Clean(c.ShareDir)
	full := filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/dl/")))
	rel, err := filepath.Rel(base, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.Error(w, "forbidden", 403)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(st.Name()))
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func main() {
	loadConfig()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page("phone.html")(w, r)
	})
	mux.HandleFunc("/upload", upload)
	mux.HandleFunc("/api/list", list)
	mux.HandleFunc("/dl/", download)
	mux.HandleFunc("/admin", local(page("admin.html")))
	mux.HandleFunc("/admin/state", local(adminState))
	mux.HandleFunc("/admin/set", local(adminSet))
	mux.HandleFunc("/admin/browse", local(adminBrowse))
	mux.HandleFunc("/admin/qr.png", local(adminQR))
	handler = mux

	p, err := start(get().Port)
	if err != nil {
		fmt.Printf("Port %d busy, using a random free port. Change it from the admin page.\n", get().Port)
		if p, err = start(0); err != nil {
			fmt.Println("Cannot start:", err)
			fmt.Scanln()
			return
		}
	}
	curPort.Store(int32(p))
	admin := fmt.Sprintf("http://localhost:%d/admin", p)
	fmt.Println("shareUp running. Admin:", admin)
	fmt.Println("Close this window to stop.")
	openBrowser(admin)
	select {}
}
