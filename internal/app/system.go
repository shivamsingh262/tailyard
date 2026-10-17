package app

import (
	"os/exec"
	"runtime"

	"github.com/atotto/clipboard"
)

func CopyToClipboard(text string) error {
	return clipboard.WriteAll(text)
}

func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}
