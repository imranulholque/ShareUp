package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Injected by the GitHub workflow (${{ github.repository }} / tag), so whichever repo
// builds the app is the repo it checks for updates. No name is hardcoded.
//
//	-X main.version=<tag>  -X main.repo=<owner/name>
var (
	version = "dev"
	repo    = "" // filled automatically from the GitHub repo that builds the app
)

// uiUpdateHook lets the tray react when a new build is found (set on Windows).
var uiUpdateHook func(tag string)

type updateState struct {
	Enabled   bool   `json:"enabled"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	URL       string `json:"url"`
	Status    string `json:"status"` // idle | checking | uptodate | installing | error
	Error     string `json:"error"`
	Progress  int    `json:"progress"`
	Checked   int64  `json:"checked"`
}

var (
	umu    sync.Mutex
	us     = updateState{Status: "idle"}
	uAsset string
	uBusy  bool
)

func updateRepo() string {
	if c := get(); strings.Contains(c.UpdateRepo, "/") {
		return c.UpdateRepo
	}
	return repo
}

func updateSnapshot() updateState {
	umu.Lock()
	defer umu.Unlock()
	s := us
	s.Enabled = get().AutoUpdate
	s.Current = version
	return s
}

func assetWanted() string {
	switch runtime.GOOS {
	case "windows":
		return "shareUp.exe"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "shareUp-mac-arm64"
		}
		return "shareUp-mac-intel"
	}
	return "shareUp-linux"
}

// checkUpdate asks GitHub for the latest release. Any tag different from this build counts as new.
func checkUpdate() {
	umu.Lock()
	if uBusy || us.Status == "installing" {
		umu.Unlock()
		return
	}
	uBusy = true
	us.Status = "checking"
	us.Error = ""
	umu.Unlock()
	defer func() { umu.Lock(); uBusy = false; umu.Unlock() }()

	fail := func(msg string) {
		umu.Lock()
		us.Status, us.Error = "error", msg
		umu.Unlock()
	}
	r := updateRepo()
	if !strings.Contains(r, "/") {
		fail("This build does not know its GitHub repo (build it with the GitHub workflow).")
		return
	}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+r+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "shareUp-updater")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fail("Could not reach GitHub.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		umu.Lock()
		us.Status, us.Latest, us.Available, us.Checked = "uptodate", "", false, time.Now().Unix()
		umu.Unlock()
		return
	}
	if resp.StatusCode != 200 {
		fail(fmt.Sprintf("GitHub answered %d.", resp.StatusCode))
		return
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel) != nil || rel.Tag == "" {
		fail("Unexpected reply from GitHub.")
		return
	}
	asset := ""
	for _, a := range rel.Assets {
		if a.Name == assetWanted() {
			asset = a.URL
		}
	}
	avail := rel.Tag != version && version != "dev"
	umu.Lock()
	us.Latest, us.URL, uAsset = rel.Tag, rel.URL, asset
	us.Available = avail
	us.Status = "uptodate"
	us.Checked = time.Now().Unix()
	umu.Unlock()
	if avail && uiUpdateHook != nil {
		uiUpdateHook(rel.Tag)
	}
}

// installUpdate downloads the new build, swaps it in, and restarts.
func installUpdate() {
	umu.Lock()
	if us.Status == "installing" || !us.Available || uAsset == "" {
		if uAsset == "" && us.Available {
			us.Status, us.Error = "error", "This release has no file for your system."
		}
		umu.Unlock()
		return
	}
	us.Status, us.Error, us.Progress = "installing", "", 0
	url := uAsset
	umu.Unlock()

	fail := func(msg string) {
		umu.Lock()
		us.Status, us.Error = "error", msg
		umu.Unlock()
	}
	exe := exePath()
	tmp := exe + ".new"
	resp, err := (&http.Client{}).Get(url)
	if err != nil || resp.StatusCode != 200 {
		fail("Download failed.")
		return
	}
	defer resp.Body.Close()
	f, err := os.Create(tmp)
	if err != nil {
		fail("Cannot write next to the app: " + err.Error())
		return
	}
	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				fail("Download failed.")
				return
			}
			done += int64(n)
			if resp.ContentLength > 0 {
				umu.Lock()
				us.Progress = int(done * 100 / resp.ContentLength)
				umu.Unlock()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				f.Close()
				os.Remove(tmp)
				fail("Download interrupted.")
				return
			}
			break
		}
	}
	f.Close()
	if done < 1<<20 {
		os.Remove(tmp)
		fail("Downloaded file looks wrong.")
		return
	}
	os.Chmod(tmp, 0755)
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		fail("Cannot replace the app: " + err.Error())
		return
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		fail("Cannot replace the app: " + err.Error())
		return
	}
	args := []string{"--after-update"}
	if len(os.Args) > 1 && os.Args[1] == "--startup" {
		args = append(args, "--startup")
	}
	cmd := exec.Command(exe, args...)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		os.Rename(exe, tmp)
		os.Rename(old, exe)
		fail("Could not restart: " + err.Error())
		return
	}
	remoteStop()
	os.Exit(0)
}

func cleanupOldBuild() {
	os.Remove(exePath() + ".old")
	os.Remove(exePath() + ".new")
}

// updateLoop checks at start and then every 30 minutes while auto-update is on.
func updateLoop() {
	for {
		if get().AutoUpdate {
			checkUpdate()
		}
		for i := 0; i < 30*60; i++ {
			time.Sleep(time.Second)
		}
	}
}

func adminUpdate(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Check   bool `json:"check"`
		Install bool `json:"install"`
	}
	json.NewDecoder(r.Body).Decode(&q)
	if q.Check {
		go checkUpdate()
	}
	if q.Install {
		go installUpdate()
	}
	time.Sleep(80 * time.Millisecond)
	adminState(w, r)
}

var _ = filepath.Join
