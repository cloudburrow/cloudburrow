package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLOUDBURROW_BYOC_FAKES points bring your own cloud at local fakes only:
// an endpoint that is not on the loopback is refused, so credentials the
// fakes are given can never leave the machine.
func TestByocFakesMustBeLoopback(t *testing.T) {
	dir := t.TempDir()
	write := func(aws string) string {
		p := filepath.Join(dir, "fakes.json")
		body := `{"aws":"` + aws + `","azure":"https://127.0.0.1:9","open_url":"http://127.0.0.1:9/open","keychain":"` + dir + `/k","ssh_config":"` + dir + `/ssh_config"}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv(byocFakesEnv, write("https://ec2.us-east-1.amazonaws.com"))
	if _, _, err := loadByocFakes(); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("a real endpoint: %v", err)
	}
	if byocSSHConfig() != "" {
		t.Fatal("a refused fakes file still set the ssh config")
	}
	t.Setenv(byocFakesEnv, write("http://127.0.0.1:8"))
	f, ok, err := loadByocFakes()
	if err != nil || !ok || f.AWS != "http://127.0.0.1:8" {
		t.Fatalf("loopback fakes: %+v %v %v", f, ok, err)
	}
	if byocSSHConfig() != dir+"/ssh_config" {
		t.Fatalf("ssh config %q", byocSSHConfig())
	}
	t.Setenv(byocFakesEnv, "")
	if _, ok, err := loadByocFakes(); ok || err != nil {
		t.Fatalf("unset: %v %v", ok, err)
	}
}
