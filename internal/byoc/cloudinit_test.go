package byoc

import (
	"strings"
	"testing"
)

func TestCloudInitPinsAndKeys(t *testing.T) {
	host, err := NewKeyPair("")
	if err != nil {
		t.Fatal(err)
	}
	client, _ := NewKeyPair("cloudburrow-dev")
	out, err := RenderCloudInit(CloudInitOptions{Cloud: AWS, Instance: "dev", User: "ubuntu", AuthorizedKey: client.Public,
		HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public, AutoStopMinutes: 45})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config\n", "ssh_pwauth: false", "ssh_genkeytypes: []", "ed25519_public: '" + host.Public + "'",
		"- '" + client.Public + "'", "docker-ce='" + DockerVersion + "'", "containerd.io='" + ContainerdVersion + "'",
		DockerKeyFingerprint, KindSHA256 + "  /tmp/kind' | sha256sum -c -", "kind/releases/download/" + KindVersion + "/",
		"printf '%d\\n' 45 > " + AutoStopFile, "chown ubuntu:ubuntu " + AutoStopFile, "minutes=$(cat " + AutoStopFile, "limit=$((minutes * 60))", "shutdown -h now", "cloudburrow-idle.timer", "usermod -aG docker ubuntu", "GatewayPorts clientspecified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init lacks %q", want)
		}
	}
	for _, line := range strings.Split(host.PrivatePEM, "\n") {
		if line != "" && !strings.Contains(out, "    "+line) {
			t.Fatalf("the host key line %q is not indented under ed25519_private", line)
		}
	}
	if strings.Contains(out, "nvidia") {
		t.Error("a CPU size installs the NVIDIA driver")
	}
	if strings.Contains(out, "amazon-ssm-agent") {
		t.Error("an SSH-mode VM runs the SSM agent")
	}
	ssm, err := RenderCloudInit(CloudInitOptions{Cloud: AWS, Instance: "dev", User: "ubuntu", AuthorizedKey: client.Public,
		HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public, SSM: true})
	if err != nil || !strings.Contains(ssm, "snap start --enable amazon-ssm-agent.amazon-ssm-agent") {
		t.Errorf("an SSM-mode VM does not start the SSM agent (%v)", err)
	}
}

func TestCloudInitGPUAndAzureDeallocate(t *testing.T) {
	host, _ := NewKeyPair("")
	out, err := RenderCloudInit(CloudInitOptions{Cloud: Azure, Instance: "gpu", User: "cloudburrow", AuthorizedKey: "ssh-ed25519 AAAA test",
		HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public, GPU: true, AutoStopMinutes: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ubuntu-drivers install --gpgpu " + NVIDIADriver, "nvidia-container-toolkit='" + NVIDIAToolkitVersion + "'",
		NVIDIAKeyFingerprint, "nvidia-ctk runtime configure --runtime=docker", "/deallocate?api-version=", "metadata/identity/oauth2/token", "|| shutdown -h now"} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init lacks %q", want)
		}
	}
}

func TestCloudInitWithoutAutoStopAndRefusals(t *testing.T) {
	host, _ := NewKeyPair("")
	out, err := RenderCloudInit(CloudInitOptions{Cloud: AWS, User: "ubuntu", AuthorizedKey: "ssh-ed25519 AAAA", HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public})
	// Auto-stop 0 is "never", written to the setting the idle check
	// reads, so it can be turned on after create.
	if err != nil || !strings.Contains(out, "printf '%d\\n' 0 > "+AutoStopFile) || !strings.Contains(out, `if [ "$minutes" -eq 0 ]; then exit 0; fi`) {
		t.Fatalf("auto-stop 0 is not written as never: %v", err)
	}
	if _, err := RenderCloudInit(CloudInitOptions{User: "bad user", AuthorizedKey: "k", HostKeyPrivate: "p", HostKeyPublic: "q"}); err == nil {
		t.Error("a user with a space was accepted")
	}
	if _, err := RenderCloudInit(CloudInitOptions{User: "ubuntu"}); err == nil {
		t.Error("cloud-init without keys was accepted")
	}
}

func TestClosestRegionAndZoneTable(t *testing.T) {
	rs := []Region{{Name: "us-east-1", Latitude: 38.9, Longitude: -77.4}, {Name: "ap-southeast-2", Latitude: -33.9, Longitude: 151.2}, {Name: "eu-west-1", Latitude: 53.3, Longitude: -6.3}}
	if got := ClosestRegion(rs, -37.8, 144.9); got != "ap-southeast-2" {
		t.Errorf("from Melbourne: %s", got)
	}
	if got := ClosestRegion([]Region{{Name: "x"}}, 0, 1); got != "x" {
		t.Errorf("no coordinates: %s", got)
	}
	tab := "# comment\nGB\t+513030-0000731\tEurope/London\nUS\t+404251-0740023\tAmerica/New_York\tEastern\n"
	lat, lon, ok := findZone(bufioScanner(tab), "America/New_York")
	if !ok || lat < 40.7 || lat > 40.72 || lon > -74.0 || lon < -74.01 {
		t.Errorf("New York = %v,%v,%v", lat, lon, ok)
	}
	if _, _, ok := findZone(bufioScanner(tab), "Mars/Base"); ok {
		t.Error("an unknown zone was found")
	}
	if lat, lon, ok := parseISO6709("-3352+15113"); !ok || lat > -33.8 || lon < 151.2 {
		t.Errorf("Sydney = %v,%v,%v", lat, lon, ok)
	}
}

func TestKeyPair(t *testing.T) {
	k, err := NewKeyPair("cb")
	if err != nil || !strings.HasPrefix(k.Public, "ssh-ed25519 ") || !strings.HasSuffix(k.Public, " cb") || !strings.Contains(k.PrivatePEM, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("%+v %v", k, err)
	}
}
