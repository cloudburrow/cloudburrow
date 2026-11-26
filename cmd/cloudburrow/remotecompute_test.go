package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

func stubProvisioned(t *testing.T, f func(config.Config) (byoc.RemoteTarget, error)) {
	t.Helper()
	prev := provisionedTarget
	provisionedTarget = f
	t.Cleanup(func() { provisionedTarget = prev })
}

// An instance whose compute is a provisioned VM (#2209) reaches it with the
// provisioner's key and pinned host key: when the provisioner cannot give
// them, the command fails naming the VM, and never falls back to a key
// file of its own.
func TestProvisionedComputeNeedsTheProvisionersKey(t *testing.T) {
	cfg := config.Config{Name: "vmtest", StateDir: t.TempDir()}
	if err := remotecompute.SaveRecord(cfg.InstanceDir(), remotecompute.Record{
		Target: remotecompute.Target{User: "ubuntu", Host: "198.51.100.7", Port: 22}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	stubProvisioned(t, func(config.Config) (byoc.RemoteTarget, error) {
		return byoc.RemoteTarget{}, errors.New("vmtest-key: not in the keychain")
	})
	var out bytes.Buffer
	_, err := connectCompute(context.Background(), cfg, &out, true)
	if err == nil || !strings.Contains(err.Error(), "the VM at ssh://ubuntu@198.51.100.7:22") || !strings.Contains(err.Error(), "not in the keychain") {
		t.Fatalf("connect = %v", err)
	}
	if _, err := os.Stat(cfg.InstanceDir() + "/remote"); !os.IsNotExist(err) {
		t.Errorf("a key directory was made for a provisioned VM: %v", err)
	}
}

// With the stand-in host (scripts/remote-compute-standin.sh), an instance
// whose record is a provisioned VM connects with the key the provisioner
// gives, against the pinned host key and the provisioner's latest address,
// and this process's docker then reaches the stand-in's Docker.
func TestProvisionedComputeRunsDockerOnTheVM(t *testing.T) {
	standIn, raw := os.Getenv("CLOUDBURROW_TEST_REMOTE_STANDIN"), os.Getenv("CLOUDBURROW_TEST_REMOTE_HOST")
	if standIn == "" || raw == "" {
		t.Skip("CLOUDBURROW_TEST_REMOTE_STANDIN and CLOUDBURROW_TEST_REMOTE_HOST name no stand-in remote host")
	}
	host, err := remotecompute.ParseTarget(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	local := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// The VM's key, as the provisioner keeps it in the keychain and
	// cloud-init installs its public half.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	spub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spub)))
	local(line+"\n", "exec", "-i", standIn, "sh", "-c", "cat >> /home/"+host.User+"/.ssh/authorized_keys")
	t.Cleanup(func() {
		_, _ = exec.Command("docker", "exec", standIn, "sed", "-i", "\\#"+line+"#d", "/home/"+host.User+"/.ssh/authorized_keys").CombinedOutput()
	})
	hostKey := local("", "exec", standIn, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")

	cfg := config.Config{Name: "vmtest", StateDir: t.TempDir()}
	// The recorded address is stale: the provisioner's is the VM's now.
	if err := remotecompute.SaveRecord(cfg.InstanceDir(), remotecompute.Record{
		Target: remotecompute.Target{User: host.User, Host: "198.51.100.7", Port: 22}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	stubProvisioned(t, func(config.Config) (byoc.RemoteTarget, error) {
		return byoc.RemoteTarget{Instance: "vmtest", Host: host.Host, Port: host.Port, User: host.User,
			PrivateKey: pem.EncodeToMemory(block), HostKey: hostKey}, nil
	})
	var out bytes.Buffer
	c, err := connectCompute(ctx, cfg, &out, true)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out.String())
	}
	defer c.Close()
	got, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ID}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker info through DOCKER_HOST=%s: %v %s", os.Getenv("DOCKER_HOST"), err, got)
	}
	if want := local("", "exec", standIn, "docker", "info", "--format", "{{.ID}}"); strings.TrimSpace(string(got)) != want {
		t.Fatalf("this process's docker reaches %q, the stand-in's is %q", got, want)
	}
	rec, _, _ := remotecompute.LoadRecord(cfg.InstanceDir())
	if !rec.Provisioned || rec.Target.Host != host.Host || rec.Target.Port != host.Port {
		t.Errorf("the record after connect = %+v, want the VM's current address, still provisioned", rec)
	}
	if _, err := os.Stat(cfg.InstanceDir() + "/remote"); !os.IsNotExist(err) {
		t.Errorf("a key directory was made for a provisioned VM: %v", err)
	}
}
