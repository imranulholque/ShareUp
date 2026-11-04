package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Internet access = a Cloudflare "quick tunnel" (no account needed).
// - The tunnel points at a SEPARATE loopback listener (rPort) that has no admin routes
//   and requires a private key, so the internet can never reach /admin.
// - It is never saved to the config: it is always OFF after a restart.

type remoteState struct {
	Status   string `json:"status"` // off | downloading | starting | on | error
	URL      string `json:"url"`
	Link     string `json:"link"`
	Progress int    `json:"progress"`
	Error    string `json:"error"`
}

var (
	rmu    sync.Mutex
	rs     = remoteState{Status: "off"}
	rToken string
	rCmd   *exec.Cmd
	rGen   int
	rPort  int
)

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func remoteSnapshot() remoteState {
	rmu.Lock()
	defer rmu.Unlock()
	s := rs
	if s.URL != "" && s.Status == "on" {
		s.Link = s.URL + "/?k=" + rToken
	}
	return s
}

func rset(gen int, f func(*remoteState)) bool {
	rmu.Lock()
	defer rmu.Unlock()
	if gen != rGen {
		return false
	}
	f(&rs)
	return true
}

func remoteStart() {
	rmu.Lock()
	if rs.Status != "off" && rs.Status != "error" {
		rmu.Unlock()
		return
	}
	rGen++
	gen := rGen
	rToken = newToken()
	rs = remoteState{Status: "starting"}
	rmu.Unlock()
	go runTunnel(gen)
}

func remoteStop() {
	rmu.Lock()
	rGen++
	cmd := rCmd
	rCmd = nil
	rToken = ""
	rs = remoteState{Status: "off"}
	rmu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// remoteRenew makes a new private key; the old link and old cookies stop working.
func remoteRenew() {
	rmu.Lock()
	if rs.Status == "on" {
		rToken = newToken()
	}
	rmu.Unlock()
}

var tunRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// sniffer reads cloudflared's log and reports the URL once the tunnel is registered.
type sniffer struct {
	mu   sync.Mutex
	acc  string
	url  string
	sent bool
	ch   chan string
}

func (s *sniffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acc += string(p)
	if s.url == "" {
		for _, m := range tunRe.FindAllString(s.acc, -1) {
			if m != "https://api.trycloudflare.com" {
				s.url = m
				break
			}
		}
	}
	if s.url != "" && !s.sent && strings.Contains(s.acc, "Registered tunnel connection") {
		s.sent = true
		s.ch <- s.url
	}
	if len(s.acc) > 4096 {
		s.acc = s.acc[len(s.acc)-2048:]
	}
	return len(p), nil
}

func runTunnel(gen int) {
	fail := func(msg string) {
		rset(gen, func(s *remoteState) { *s = remoteState{Status: "error", Error: msg} })
	}
	bin, err := ensureCloudflared(gen)
	if err != nil {
		fail("Could not download the connection helper: " + err.Error())
		return
	}
	if !rset(gen, func(s *remoteState) { s.Status = "starting" }) {
		return
	}
	cmd := exec.Command(bin, "tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", rPort),
		"--no-autoupdate", "--protocol", "http2")
	hideWindow(cmd)
	sn := &sniffer{ch: make(chan string, 1)}
	cmd.Stderr = sn
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	rmu.Lock()
	if gen != rGen {
		rmu.Unlock()
		cmd.Process.Kill()
		cmd.Wait()
		return
	}
	rCmd = cmd
	rmu.Unlock()

	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	select {
	case u := <-sn.ch:
		time.Sleep(1500 * time.Millisecond) // let DNS settle
		if !rset(gen, func(s *remoteState) { s.Status = "on"; s.URL = u }) {
			cmd.Process.Kill()
			return
		}
	case <-done:
		fail("The connection helper stopped. Check your internet.")
		return
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		fail("Timed out. Check your internet connection.")
		return
	}
	<-done
	rset(gen, func(s *remoteState) {
		if s.Status == "on" {
			*s = remoteState{Status: "error", Error: "Connection lost."}
		}
	})
}

type progW struct {
	n, total int64
	gen      int
}

func (p *progW) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.total > 0 {
		pc := int(p.n * 100 / p.total)
		rset(p.gen, func(s *remoteState) {
			if s.Status == "downloading" {
				s.Progress = pc
			}
		})
	}
	return len(b), nil
}

func cfSource() (string, bool, error) {
	base := "https://github.com/cloudflare/cloudflared/releases/latest/download/"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return base + "cloudflared-windows-amd64.exe", false, nil
	case "linux/amd64":
		return base + "cloudflared-linux-amd64", false, nil
	case "linux/arm64":
		return base + "cloudflared-linux-arm64", false, nil
	case "darwin/amd64":
		return base + "cloudflared-darwin-amd64.tgz", true, nil
	case "darwin/arm64":
		return base + "cloudflared-darwin-arm64.tgz", true, nil
	}
	return "", false, fmt.Errorf("unsupported system %s/%s", runtime.GOOS, runtime.GOARCH)
}

// ensureCloudflared downloads the helper once, next to the app.
func ensureCloudflared(gen int) (string, error) {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(filepath.Dir(cfgPath), name)
	if st, err := os.Stat(bin); err == nil && st.Size() > 1<<20 {
		return bin, nil
	}
	src, isTgz, err := cfSource()
	if err != nil {
		return "", err
	}
	rset(gen, func(s *remoteState) { s.Status = "downloading"; s.Progress = 0 })
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(src)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := bin + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", err
	}
	body := io.TeeReader(resp.Body, &progW{total: resp.ContentLength, gen: gen})
	if isTgz {
		err = extractTgz(body, out)
	} else {
		_, err = io.Copy(out, body)
	}
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	os.Chmod(tmp, 0755)
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	return bin, nil
}

func extractTgz(r io.Reader, out io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("helper not found in archive")
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) == "cloudflared" {
			_, err = io.Copy(out, tr)
			return err
		}
	}
}

// remoteAuth guards everything that arrives through the tunnel.
// First visit: /?k=KEY sets a cookie and redirects. After that the cookie is required.
func remoteAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rmu.Lock()
		tok, on := rToken, rs.Status == "on"
		rmu.Unlock()
		if !on || tok == "" {
			http.Error(w, "Internet access is off.", 503)
			return
		}
		eq := func(a string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(tok)) == 1 }
		if k := r.URL.Query().Get("k"); k != "" && eq(k) {
			http.SetCookie(w, &http.Cookie{Name: "su_k", Value: tok, Path: "/", HttpOnly: true,
				Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
			q := r.URL.Query()
			q.Del("k")
			dest := r.URL.Path
			if e := q.Encode(); e != "" {
				dest += "?" + e
			}
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		c, err := r.Cookie("su_k")
		if err != nil || !eq(c.Value) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(401)
			fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><body style="font-family:system-ui;text-align:center;margin-top:20vh;padding:0 20px"><h2>Access denied</h2><p>Scan the QR code in the shareUp window on your PC again.</p>`)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func startRemoteListener(h http.Handler) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	rPort = ln.Addr().(*net.TCPAddr).Port
	s := &http.Server{Handler: remoteAuth(h), ReadHeaderTimeout: 10 * time.Second}
	go s.Serve(ln)
	return nil
}
