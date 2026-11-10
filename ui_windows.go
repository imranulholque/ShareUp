//go:build windows

package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"

	"fyne.io/systray"
	"github.com/jchv/go-webview2"
)

//go:embed icon.ico
var trayIcon []byte

var (
	user32         = syscall.NewLazyDLL("user32.dll")
	pShowWindow    = user32.NewProc("ShowWindow")
	pSetForeground = user32.NewProc("SetForegroundWindow")
	pIsIconic      = user32.NewProc("IsIconic")
	winMu          sync.Mutex
	winHwnd        uintptr // 0 while the window is closed (app lives in the tray)
	showCh         = make(chan struct{}, 1)
	updItem        *systray.MenuItem
)

const (
	swRestore = 9
	swShow    = 5
)

// uiShow opens the window, or brings it to the front if it is already open.
func uiShow() {
	winMu.Lock()
	h := winHwnd
	winMu.Unlock()
	if h != 0 {
		if r, _, _ := pIsIconic.Call(h); r != 0 {
			pShowWindow.Call(h, swRestore)
		} else {
			pShowWindow.Call(h, swShow)
		}
		pSetForeground.Call(h)
		return
	}
	select {
	case showCh <- struct{}{}:
	default:
	}
}

// windowLoop owns one OS thread (WebView2 needs that). Closing the window only
// destroys the view; the app keeps running in the tray until "Quit".
func windowLoop() {
	runtime.LockOSThread()
	for range showCh {
		openWindow()
	}
}

func openWindow() {
	data := filepath.Join(os.Getenv("LOCALAPPDATA"), "shareUp", "webview")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  data,
		WindowOptions: webview2.WindowOptions{
			Title:  "shareUp",
			Width:  900,
			Height: 760,
			IconId: 2, // the icon embedded in the exe by the build
			Center: true,
		},
	})
	if w == nil { // WebView2 runtime missing: fall back to the browser
		openBrowser(adminURL())
		return
	}
	defer w.Destroy()
	w.SetSize(420, 520, webview2.HintMin) // resizable, with a sensible minimum
	winMu.Lock()
	winHwnd = uintptr(w.Window())
	winMu.Unlock()
	w.Navigate(adminURL())
	w.Run()
	winMu.Lock()
	winHwnd = 0
	winMu.Unlock()
}

func runUI(silent bool) {
	go windowLoop()
	if !silent {
		uiShow()
	}
	uiUpdateHook = func(tag string) {
		if updItem != nil {
			updItem.SetTitle("Update available: " + tag)
			updItem.Show()
		}
		systray.SetTooltip("shareUp – update available (" + tag + ")")
	}
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("shareUp")
		systray.SetTooltip("shareUp")
		systray.SetOnTapped(uiShow) // left-click the tray icon opens the window
		open := systray.AddMenuItem("Open shareUp", "Show the window")
		updItem = systray.AddMenuItem("", "Open shareUp to install the update")
		updItem.Hide()
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit shareUp", "Stop shareUp")
		go func() {
			for {
				select {
				case <-open.ClickedCh:
					uiShow()
				case <-updItem.ClickedCh:
					uiShow()
				case <-quit.ClickedCh:
					quitApp()
				}
			}
		}()
		if u := updateSnapshot(); u.Available {
			uiUpdateHook(u.Latest)
		}
	}, nil)
}
