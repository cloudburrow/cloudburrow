package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every flag `cloudburrow up --help` prints, up's own -detach, -offline and
// the rest as well as the shared configuration flags, has a row in
// docs/configuration.md's Settings table, and every row there is a flag up
// takes (#710). internal/config's TestConfigurationDocCoversEveryFlagAndVariable
// checks the configuration flags' cells and variables.
func TestConfigurationDocCoversUpOwnFlags(t *testing.T) {
	var help bytes.Buffer
	if err := printCommandHelp(&help, "up"); err != nil {
		t.Fatal(err)
	}
	printed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  -([a-z][a-z0-9-]*)`).FindAllStringSubmatch(help.String(), -1) {
		printed[m[1]] = true
	}
	if len(printed) == 0 {
		t.Fatal("up --help printed no flags")
	}

	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "\n## Settings\n")
	if start < 0 {
		t.Fatal("docs/configuration.md: no \"## Settings\" section")
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	rows := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `--([a-z][a-z0-9-]*)` \\|").FindAllStringSubmatch(section, -1) {
		rows[m[1]] = true
	}

	for f := range printed {
		if !rows[f] {
			t.Errorf("up --help prints -%s, which has no row in docs/configuration.md's Settings table", f)
		}
	}
	for f := range rows {
		if !printed[f] {
			t.Errorf("docs/configuration.md's Settings table has a row for --%s, which up --help does not print", f)
		}
	}
}
