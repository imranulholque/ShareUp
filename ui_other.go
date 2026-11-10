//go:build !windows

package main

// Non-Windows systems keep the original behaviour: use the default browser.
func runUI(silent bool) {
	if !silent {
		openBrowser(adminURL())
	}
	select {}
}

func uiShow() { openBrowser(adminURL()) }
