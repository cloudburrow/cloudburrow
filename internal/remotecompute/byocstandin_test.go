package remotecompute

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

// envStandInContainer names the stand-in's container on this machine's
// Docker (scripts/remote-compute-standin.sh prints it).
const envStandInContainer = "CLOUDBURROW_TEST_REMOTE_STANDIN"

// localDocker runs docker against this machine's own Docker.
func localDocker(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// cloudInitHostKey is the VM's host key the provisioner put in its
// cloud-init, as cloud-init would install it.
func cloudInitHostKey(t *testing.T, userData string) (private, public string) {
	t.Helper()
	var b strings.Builder
	in := false
	for _, line := range strings.Split(userData, "\n") {
		switch {
		case strings.HasPrefix(line, "  ed25519_private: |"):
			in = true
		case in && strings.HasPrefix(line, "    "):
			b.WriteString(strings.TrimPrefix(line, "    ") + "\n")
		default:
			in = false
		}
	}
	raw, err := ssh.ParseRawPrivateKey([]byte(b.String()))
	if err != nil {
		t.Fatalf("the cloud-init carries no host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b.String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// TestProvisionedVMBecomesTheInstancesCompute (#2209, phases 1 to 3): the
// provisioner's fake cloud creates a VM, and the stand-in host boots as it,
// with the host key and CloudBurrow's public key the cloud-init carries.
// The manager hands the VM to the Engine, which signs in with the key from
// the keychain (no key file is written) against the pinned host key, and
// records the VM as the instance's compute: Docker through the connection
// is the stand-in's. Delete closes the connection, forgets the record, and
// the keychain item and every tagged resource are gone.
func TestProvisionedVMBecomesTheInstancesCompute(t *testing.T) {
	standInName := os.Getenv(envStandInContainer)
	raw := os.Getenv(envRemoteHost)
	if standInName == "" || raw == "" {
		t.Skipf("%s and %s name no stand-in remote host", envStandInContainer, envRemoteHost)
	}
	host, err := ParseTarget(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cloud := byoctest.New(byoc.AWS)
	var pinned string
	cloud.Boot = func(spec byoc.MachineSpec, m *byoc.Machine) error {
		priv, pub := cloudInitHostKey(t, spec.UserData)
		pinned = pub
		u := spec.SSHUser
		script := fmt.Sprintf(`set -e
id %[1]s >/dev/null 2>&1 || { adduser -D -s /bin/sh %[1]s; addgroup %[1]s docker; sed -i 's/^%[1]s:!/%[1]s:*/' /etc/shadow; }
install -d -m 700 -o %[1]s -g %[1]s /home/%[1]s/.ssh
printf '%%s\n' %[2]s > /home/%[1]s/.ssh/authorized_keys
chown %[1]s:%[1]s /home/%[1]s/.ssh/authorized_keys; chmod 600 /home/%[1]s/.ssh/authorized_keys
cat > /etc/ssh/ssh_host_ed25519_key; chmod 600 /etc/ssh/ssh_host_ed25519_key
printf '%%s\n' %[3]s > /etc/ssh/ssh_host_ed25519_key.pub
kill -HUP "$(cat /var/run/sshd.pid)"`, u, shellQuote(spec.SSHPublicKey), shellQuote(pub))
		if out, err := localDocker(ctx, priv, "exec", "-i", standInName, "sh", "-c", script); err != nil {
			return fmt.Errorf("booting the stand-in as the VM: %v %s", err, out)
		}
		time.Sleep(time.Second) // sshd re-executes on HUP
		m.PublicIP, m.SSHPort = host.Host, host.Port
		return nil
	}
	keys := keychain.NewMemory()
	stateDir := t.TempDir()
	engine := &Engine{StateDir: stateDir}
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: keys, Drivers: []byoc.Driver{cloud}, Engine: engine,
		IPLookup:    func(context.Context) (string, error) { return "203.0.113.9", nil },
		OpenBrowser: func(string) error { return nil },
		Location:    func() (float64, float64, bool) { return 38.9, -77.4, true }})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: "AKIDSTANDIN", SecretAccessKey: "standin"})
	if err != nil {
		t.Fatal(err)
	}
	events := engine.Events("rcvm")

	op, err := m.Create(ctx, byoc.CreateRequest{Instance: "rcvm", Connection: conn.ID, Region: "us-east-1", Preset: byoc.Small}, true, nil)
	if err != nil || op.Error != "" {
		t.Fatalf("Create = %v %q", err, op.Error)
	}
	var remote string
	for _, e := range op.Events {
		if e.Step == "remote" {
			remote = string(e.Status) + ": " + e.Message
		}
	}
	if !strings.HasPrefix(remote, string(byoc.StepDone)) {
		t.Fatalf("the create's hand-over step = %q", remote)
	}
	st, err := engine.Status(ctx, "rcvm")
	if err != nil || !st.Connected {
		t.Fatalf("engine status = %+v, %v", st, err)
	}
	select {
	case e := <-events:
		t.Logf("first engine event: %s %s", e.Kind, e.Message)
	default:
		t.Error("the engine sent no events")
	}

	// The instance's record names the VM, provisioned.
	rec, had, err := LoadRecord(filepath.Join(stateDir, "rcvm"))
	if err != nil || !had || !rec.Provisioned || rec.Target.Host != host.Host || rec.Target.Port != host.Port || rec.Target.User != "ubuntu" {
		t.Fatalf("record = %+v %v %v", rec, had, err)
	}
	// Docker through the connection is the stand-in's.
	s := engine.session("rcvm")
	viaSSH, err := s.docker(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatal(err)
	}
	inside, err := localDocker(ctx, "", "exec", standInName, "docker", "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(viaSSH) != strings.TrimSpace(inside) || strings.TrimSpace(viaSSH) == "" {
		t.Fatalf("Docker through the connection is %q, the stand-in's is %q", viaSSH, inside)
	}
	// The key never touched the disk: no file under the state directory or
	// the connection's run directory holds a private key.
	for _, root := range []string{stateDir, s.runDir} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), "PRIVATE KEY") {
					t.Errorf("%s holds a private key", p)
				}
			}
			return nil
		})
	}
	// A host key other than the pinned one is refused.
	vm, err := m.RemoteTarget("rcvm")
	if err != nil {
		t.Fatal(err)
	}
	if vm.HostKey != pinned {
		t.Errorf("the provisioner pinned %q, the VM's cloud-init has %q", vm.HostKey, pinned)
	}
	_, other := newPEM(t)
	if _, err := Connect(ctx, Options{Target: rec.Target, StateDir: t.TempDir(), Instance: "x", SkipPrerequisites: true,
		Key: PEMKey(vm.PrivateKey), HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other)))}); err == nil {
		t.Error("connected to a host whose key is not the pinned one")
	} else {
		t.Logf("another host key is refused: %v", err)
	}
	if pinned == "" {
		t.Fatal("the stand-in was never booted")
	}
	if err := engine.Reconnect(ctx, "rcvm"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	op, err = m.Delete(ctx, "rcvm", true, nil)
	if err != nil || op.Error != "" {
		t.Fatalf("Delete = %v %q", err, op.Error)
	}
	if _, had, _ := LoadRecord(filepath.Join(stateDir, "rcvm")); had {
		t.Error("the record of the deleted VM stays")
	}
	if engine.session("rcvm") != nil {
		t.Error("the engine still holds the deleted VM's connection")
	}
	if names := keys.Names(); len(names) != 1 {
		// The connection's credentials stay; the VM's key goes.
		t.Errorf("keychain items after Delete: %v", names)
	}
	if r := cloud.Resources("rcvm"); len(r) != 0 {
		t.Errorf("resources left: %v", r)
	}
}
