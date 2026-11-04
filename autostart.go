package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// Cached so the admin page can poll /admin/state without spawning processes.
var autoOn atomic.Bool

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func exePath() string {
	p, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func autostartFile() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "com.shareup.app.plist")
	}
	return filepath.Join(home, ".config", "autostart", "shareup.desktop")
}

func autostartEnabled() bool {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("reg", "query", runKey, "/v", "shareUp")
		hideWindow(cmd)
		return cmd.Run() == nil
	}
	_, err := os.Stat(autostartFile())
	return err == nil
}

// setAutostart adds or removes the "start when I sign in" entry.
// The app is launched with --startup so it stays quiet (no browser window).
func setAutostart(on bool) error {
	exe := exePath()
	if runtime.GOOS == "windows" {
		var cmd *exec.Cmd
		if on {
			cmd = exec.Command("reg", "add", runKey, "/v", "shareUp", "/t", "REG_SZ", "/d", `"`+exe+`" --startup`, "/f")
		} else {
			cmd = exec.Command("reg", "delete", runKey, "/v", "shareUp", "/f")
		}
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil && on {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	f := autostartFile()
	if !on {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f), 0755); err != nil {
		return err
	}
	var body string
	if runtime.GOOS == "darwin" {
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(exe)
		body = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.shareup.app</string>
<key>ProgramArguments</key><array><string>` + esc + `</string><string>--startup</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`
	} else {
		body = "[Desktop Entry]\nType=Application\nName=shareUp\nExec=\"" + exe + "\" --startup\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	}
	return os.WriteFile(f, []byte(body), 0644)
}
