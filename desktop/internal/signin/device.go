package signin

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Platform is the device's platform as the server names it.
func Platform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	}
	return "linux"
}

// DeviceName is the default name of this device in the user's device
// list: on macOS the computer's name as the user set it («MacBook
// Василия»), which the hostname is only a mangled form of; elsewhere the
// hostname.
func DeviceName() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				return name
			}
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return strings.TrimSuffix(name, ".local")
	}
	return "Компьютер"
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// Not "cmd /c start": cmd would read the & in the query as a
		// command separator.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// xdg-open may stay until the browser exits; reap it whenever.
	go func() { _ = cmd.Wait() }()
	return nil
}
