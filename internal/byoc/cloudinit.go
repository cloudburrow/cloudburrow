package byoc

import (
	"fmt"
	"strings"
)

// The software cloud-init installs on a VM, pinned (dependencies.json,
// "byoc"). Docker comes from Docker's apt repository, whose signing key is
// checked against its published fingerprint; kind is the release CloudBurrow
// runs locally, checked against the release's sha256.
const (
	DockerVersion        = "5:29.9.3-1~ubuntu.24.04~noble"
	ContainerdVersion    = "2.4.2-4~ubuntu.24.04~noble"
	DockerKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	KindVersion          = "v0.33.0"
	KindSHA256           = "aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d" // kind-linux-amd64
	// NVIDIADriver is the Ubuntu archive's server driver branch for the GPU
	// presets, installed with ubuntu-drivers, which picks the kernel's
	// matching signed modules.
	NVIDIADriver = "nvidia:580-server"
	// NVIDIAToolkitVersion is the NVIDIA Container Toolkit, from NVIDIA's
	// apt repository, its key checked against NVIDIAKeyFingerprint.
	NVIDIAToolkitVersion = "1.20.2-1"
	NVIDIAKeyFingerprint = "C95B321B61E88C1809C4F759DDCAE044F796ECB0"
)

// CloudInitOptions is what RenderCloudInit writes.
type CloudInitOptions struct {
	Cloud    Cloud
	Instance string
	// User is the login the authorized key opens: the image's default user
	// on AWS (ubuntu), the VM's admin user on Azure.
	User string
	// AuthorizedKey is CloudBurrow's public key, authorized_keys form.
	AuthorizedKey string
	// HostKeyPrivate and HostKeyPublic are the VM's ed25519 host key, made
	// on the user's machine so the first connection is verified rather
	// than trusted.
	HostKeyPrivate string
	HostKeyPublic  string
	GPU            bool
	// AutoStopMinutes stops the VM after that long with no SSH connection
	// (the remote engine's tunnel is one); 0 never.
	AutoStopMinutes int
}

// RenderCloudInit is the VM's cloud-init user data.
func RenderCloudInit(o CloudInitOptions) (string, error) {
	if o.User == "" || o.AuthorizedKey == "" || o.HostKeyPrivate == "" || o.HostKeyPublic == "" {
		return "", fmt.Errorf("cloud-init needs a user, an authorized key and a host key")
	}
	if strings.ContainsAny(o.User, " \n:'\"") {
		return "", fmt.Errorf("invalid user %q", o.User)
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("#cloud-config")
	w("# CloudBurrow instance %s (#2209): Docker and kind for its compute; nothing else listens.", yamlComment(o.Instance))
	w("ssh_pwauth: false")
	w("disable_root: true")
	w("ssh_authorized_keys:")
	w("  - %s", yamlQuote(o.AuthorizedKey))
	w("ssh_deletekeys: true")
	w("ssh_genkeytypes: []")
	w("ssh_keys:")
	w("  ed25519_private: |")
	for _, line := range strings.Split(strings.TrimRight(o.HostKeyPrivate, "\n"), "\n") {
		w("    %s", line)
	}
	w("  ed25519_public: %s", yamlQuote(strings.TrimSpace(o.HostKeyPublic)))
	w("package_update: true")
	w("packages: [ca-certificates, curl, gnupg, python3]")
	w("write_files:")
	file := func(path, mode, content string) {
		w("  - path: %s", path)
		w("    permissions: %s", yamlQuote(mode))
		w("    owner: root:root")
		w("    content: |")
		for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
			w("      %s", line)
		}
	}
	file("/usr/local/sbin/cloudburrow-setup", "0755", setupScript(o))
	if o.AutoStopMinutes > 0 {
		file("/usr/local/sbin/cloudburrow-idle", "0755", idleScript(o))
		file("/etc/systemd/system/cloudburrow-idle.service", "0644",
			"[Unit]\nDescription=CloudBurrow auto-stop check\n\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/cloudburrow-idle\n")
		file("/etc/systemd/system/cloudburrow-idle.timer", "0644",
			"[Unit]\nDescription=CloudBurrow auto-stop check, every minute\n\n[Timer]\nOnBootSec=2min\nOnUnitActiveSec=1min\n\n[Install]\nWantedBy=timers.target\n")
	}
	w("runcmd:")
	w("  - [bash, -c, '/usr/local/sbin/cloudburrow-setup > /var/log/cloudburrow-setup.log 2>&1']")
	return b.String(), nil
}

func setupScript(o CloudInitOptions) string {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
# CloudBurrow's VM setup (#2209): pinned Docker and kind` + map[bool]string{true: ", the NVIDIA driver and container toolkit", false: ""}[o.GPU] + `.
set -euo pipefail
mkdir -p /var/lib/cloudburrow
trap 'echo failed > /var/lib/cloudburrow/setup; exit 1' ERR
export DEBIAN_FRONTEND=noninteractive
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc | grep -q '^fpr:::::::::` + DockerKeyFingerprint + `:'
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce='` + DockerVersion + `' docker-ce-cli='` + DockerVersion + `' containerd.io='` + ContainerdVersion + `'
apt-mark hold docker-ce docker-ce-cli containerd.io
usermod -aG docker ` + o.User + `
curl -fsSLo /tmp/kind https://github.com/kubernetes-sigs/kind/releases/download/` + KindVersion + `/kind-linux-amd64
echo '` + KindSHA256 + `  /tmp/kind' | sha256sum -c -
install -m 0755 /tmp/kind /usr/local/bin/kind
rm -f /tmp/kind
`)
	if o.GPU {
		b.WriteString(`apt-get install -y ubuntu-drivers-common
ubuntu-drivers install --gpgpu ` + NVIDIADriver + `
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey -o /etc/apt/keyrings/nvidia-container-toolkit.asc
gpg --show-keys --with-colons /etc/apt/keyrings/nvidia-container-toolkit.asc | grep -q '^fpr:::::::::` + NVIDIAKeyFingerprint + `:'
echo 'deb [signed-by=/etc/apt/keyrings/nvidia-container-toolkit.asc] https://nvidia.github.io/libnvidia-container/stable/deb/$(ARCH) /' > /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update
apt-get install -y nvidia-container-toolkit='` + NVIDIAToolkitVersion + `' nvidia-container-toolkit-base='` + NVIDIAToolkitVersion + `' libnvidia-container-tools='` + NVIDIAToolkitVersion + `' libnvidia-container1='` + NVIDIAToolkitVersion + `'
apt-mark hold nvidia-container-toolkit nvidia-container-toolkit-base libnvidia-container-tools libnvidia-container1
nvidia-ctk runtime configure --runtime=docker --set-as-default
nvidia-ctk config --in-place --set accept-nvidia-visible-devices-as-volume-mounts=true
systemctl restart docker
`)
	}
	if o.AutoStopMinutes > 0 {
		b.WriteString("systemctl daemon-reload\nsystemctl enable --now cloudburrow-idle.timer\n")
	}
	b.WriteString("echo ready > /var/lib/cloudburrow/setup\n")
	if o.GPU {
		// The driver loads on the next boot.
		b.WriteString("systemctl reboot\n")
	}
	return b.String()
}

// idleScript stops the VM after AutoStopMinutes with no established SSH
// connection. Time before the latest boot does not count, so a VM started
// again is not stopped at once.
func idleScript(o CloudInitOptions) string {
	stop := "shutdown -h now"
	if o.Cloud == Azure {
		// A VM stopped from inside is still billed on Azure: deallocate
		// through the VM's own managed identity (Virtual Machine
		// Contributor on itself), and shut down only if that is refused.
		stop = `python3 - <<'EOF' || shutdown -h now
import json, urllib.request
def get(url):
    req = urllib.request.Request(url, headers={"Metadata": "true"})
    return json.load(urllib.request.urlopen(req, timeout=10))
tok = get("http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://management.azure.com/")["access_token"]
rid = get("http://169.254.169.254/metadata/instance/compute?api-version=2021-02-01")["resourceId"]
req = urllib.request.Request("https://management.azure.com" + rid + "/deallocate?api-version=2024-07-01", data=b"", method="POST",
                             headers={"Authorization": "Bearer " + tok})
urllib.request.urlopen(req, timeout=30)
EOF`
	}
	return fmt.Sprintf(`#!/bin/bash
# CloudBurrow auto-stop (#2209): stop after %d idle minutes.
set -eu
limit=%d
stamp=/var/lib/cloudburrow/last-active
mkdir -p /var/lib/cloudburrow
now=$(date +%%s)
boot=$((now - $(cut -d. -f1 /proc/uptime)))
if [ ! -f "$stamp" ] || [ -n "$(ss -Htn state established '( sport = :22 )')" ]; then
  echo "$now" > "$stamp"
  exit 0
fi
last=$(cat "$stamp")
if [ "$last" -lt "$boot" ]; then last=$boot; fi
if [ $((now - last)) -lt "$limit" ]; then exit 0; fi
logger -t cloudburrow-idle "no SSH connection for $((now - last))s: stopping"
%s
`, o.AutoStopMinutes, o.AutoStopMinutes*60, stop)
}

func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func yamlComment(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
