package compute

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// googleManagerUnit is google-guest-agent-manager.service as Google's
// package (google-guest-agent 20261021.00) installs it.
const googleManagerUnit = `[Unit]
Description=Google Compute Engine Guest Agent Plugin Manager
# Start before sshd in order to regenerate SSH host keys.
Before=sshd.service ssh.service
After=network-online.target syslog.service
After=network.service networking.service NetworkManager.service systemd-networkd.service

[Service]
Type=notify
ExecStart=/usr/bin/google_guest_agent_manager
OOMScoreAdjust=-999
Restart=always

[Install]
WantedBy=sshd.service ssh.service
WantedBy=multi-user.target
WantedBy=network.service networking.service NetworkManager.service systemd-networkd.service
`

// TestGuestAgentDoesNotHoldSSHD (#2250): the image's copy of Google's
// guest agent unit is not ordered before sshd, so an agent that cannot
// reach the metadata server yet does not hold the guest's SSH for its
// start timeout; the rest of the unit, its WantedBy= included, is the
// package's.
func TestGuestAgentDoesNotHoldSSHD(t *testing.T) {
	b, err := guestFS.ReadFile("guest/root/usr/lib/cloudburrow/guest-agent-units")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	start := strings.Index(script, "unorder_sshd() {")
	end := strings.Index(script[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("no unorder_sshd in guest-agent-units")
	}
	fn := script[start : start+end+3]
	for _, u := range []string{"google-guest-agent-manager.service", "google-guest-agent.service"} {
		if !strings.Contains(script, "unorder_sshd "+u) {
			t.Errorf("guest-agent-units does not unorder %s", u)
		}
	}
	for name, unit := range map[string]string{
		"package": googleManagerUnit,
		"mixed":   strings.Replace(googleManagerUnit, "Before=sshd.service ssh.service", "Before=sshd.service foo.service ssh.service", 1),
	} {
		lib, etc := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(lib, "m.service"), []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", "set -e\n"+fn+"unorder_sshd m.service")
		cmd.Env = append(os.Environ(), "UNIT_DIRS="+lib, "ETC_UNITS="+etc)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		got, err := os.ReadFile(filepath.Join(etc, "m.service"))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(googleManagerUnit, "# Start before sshd in order to regenerate SSH host keys.\nBefore=sshd.service ssh.service\n", "", 1)
		if name == "mixed" {
			want = strings.Replace(googleManagerUnit, "# Start before sshd in order to regenerate SSH host keys.\nBefore=sshd.service ssh.service", "Before=foo.service", 1)
		}
		if string(got) != want {
			t.Errorf("%s: the unit is\n%s\nwant\n%s", name, got, want)
		}
	}
}
