package byoc

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenURL opens an https sign-in page in the user's default browser.
func OpenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return fmt.Errorf("refusing to open %q: not an https page", raw)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u.String())
	case "linux":
		cmd = exec.Command("xdg-open", u.String())
	default:
		return fmt.Errorf("open %s in your browser", u.String())
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening the browser (open %s yourself): %w", u.String(), err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
