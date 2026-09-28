package console

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// vendoredSHA256 pins every third-party file the console embeds (#781), so
// a hand edit to a vendored copy fails here rather than shipping as the
// upstream release it no longer is. dependencies.json records the same.
var vendoredSHA256 = map[string]string{
	"assets/vendor/xterm/xterm.js":          "14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972",
	"assets/vendor/xterm/xterm.css":         "854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6",
	"assets/vendor/xterm/LICENSE":           "b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd",
	"assets/vendor/xterm/addon-fit.js":      "ba3ea256ce0620a0992a197d6c9baea64823fc93d8da07a9e366ca9943c18527",
	"assets/vendor/xterm/addon-fit.LICENSE": "e256f01188af527e4d06d21d06fbf785ae9c50d4b328bf03cbe0ba7f0aa4228f",
}

// TestVendoredAssetsAreLicensed ties every file under assets/vendor to a
// row of the licence table in assets/icons/PROVENANCE.md, with a source, a
// licence and the date it was checked, and to its pinned hash; the licence
// texts themselves ship in the binary; and NOTICE names the component.
func TestVendoredAssetsAreLicensed(t *testing.T) {
	provenance := consoleAsset(t, "icons/PROVENANCE.md")
	head := "| Path | Source | Licence | Checked |"
	i := strings.Index(provenance, head)
	if i < 0 {
		t.Fatalf("PROVENANCE.md has no licence table headed %q", head)
	}
	date := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	licensed := map[string]bool{}
	for _, line := range strings.Split(provenance[i:], "\n")[2:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 4 {
			t.Errorf("licence row %q does not have four cells", line)
			continue
		}
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		path := "assets/" + strings.Trim(cells[0], "`")
		if cells[1] == "" || cells[2] == "" || !date.MatchString(cells[3]) {
			t.Errorf("licence row for %s needs a source, a licence and a YYYY-MM-DD date", path)
		}
		licensed[path] = true
	}

	seen := 0
	err := fs.WalkDir(assets, "assets/vendor", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		seen++
		if !licensed[path] {
			t.Errorf("%s is embedded but has no row in the licence table of assets/icons/PROVENANCE.md", path)
		}
		b, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if want, ok := vendoredSHA256[path]; !ok || hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s has sha256 %x; vendoredSHA256 pins %q", path, sum, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(vendoredSHA256) {
		t.Errorf("%d vendored files embedded, %d pinned", seen, len(vendoredSHA256))
	}
	for path := range licensed {
		if _, err := assets.ReadFile(path); err != nil {
			t.Errorf("licence row for %s names a file that is not embedded", path)
		}
	}
	for _, text := range []string{"assets/vendor/xterm/LICENSE", "assets/vendor/xterm/addon-fit.LICENSE"} {
		b, _ := assets.ReadFile(text)
		if !strings.Contains(string(b), "Permission is hereby granted, free of charge") {
			t.Errorf("%s is not the MIT licence text", text)
		}
	}
	notice, err := os.ReadFile("../../NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notice), "xterm.js 6.0.0") {
		t.Error("NOTICE does not name the embedded xterm.js")
	}
}

// The terminal's emulator is served from the binary, so it works with no
// network, and it is loaded only when the drawer is opened.
func TestTheTerminalEmulatorIsServedLocally(t *testing.T) {
	t.Parallel()
	srv := serve(t)
	for _, path := range []string{"/vendor/xterm/xterm.js", "/vendor/xterm/addon-fit.js", "/vendor/xterm/xterm.css"} {
		if code, body := get(t, srv, path, nil); code != 200 || len(body) < 1000 {
			t.Errorf("GET %s = %d (%d bytes)", path, code, len(body))
		}
	}
	js := consoleAsset(t, "console.js")
	for _, want := range []string{`script("/vendor/xterm/xterm.js")`, `/api/terminal/socket`} {
		if !strings.Contains(js, want) {
			t.Errorf("console.js does not contain %s", want)
		}
	}
	if strings.Contains(consoleAsset(t, "index.html"), "xterm.js") {
		t.Error("index.html loads the emulator on every page; it is loaded when the drawer opens")
	}
}
