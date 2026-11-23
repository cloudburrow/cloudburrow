//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// TestVMMetadataServerFromInsideTheVM2177: inside a Compute Engine VM,
// metadata.google.internal, 169.254.169.254 and GCE_METADATA_HOST answer
// the VM's own metadata server, as on Compute Engine (#2177). Over SSH
// (the guest agent's push of the instance's ssh-keys), curl reads the
// instance's name, zone, attributes and network interface, its service
// account's token, an ETag, and is refused without Metadata-Flavor or with
// X-Forwarded-For; a guest attribute written in the guest is read back by
// instances.getGuestAttributes; and Google's own metadata package
// (cloud.google.com/go/compute/metadata, built for the guest) reads the
// same, a token included, and its Subscribe (wait_for_change) sees a
// setMetadata, which also pushes a second user's key.
// covers: compute.instances.getGuestAttributes
func TestVMMetadataServerFromInsideTheVM2177(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)

	// The probe, built for the guest before anything is made.
	dir := t.TempDir()
	probe := filepath.Join(dir, "vmmetadata")
	build := exec.CommandContext(ctx, "go", "build", "-o", probe, "./testdata/vmmetadata")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v: %s", err, out)
	}
	bin, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}

	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Project()
	const zone = "us-central1-a"
	vm := fmt.Sprintf("md-%d", time.Now().UnixNano()%1e6)
	sa := "vm-app@" + p + ".iam.gserviceaccount.com"
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm}); err == nil {
			_ = op.Wait(c)
		}
	})
	key := func(user string) (string, ssh.Signer) {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		sp, _ := ssh.NewPublicKey(pub)
		s, _ := ssh.NewSignerFromKey(priv)
		return user + ":" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + user, s
	}
	line1, signer1 := key("tester")
	line2, signer2 := key("second")
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vm),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{
			{Key: proto.String("role"), Value: proto.String("web")},
			{Key: proto.String("enable-guest-attributes"), Value: proto.String("TRUE")},
			{Key: proto.String("ssh-keys"), Value: proto.String(line1)}}},
		ServiceAccounts: []*computepb.ServiceAccount{{Email: proto.String(sa), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
	}}))
	inst, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vm})
	if err != nil {
		t.Fatal(err)
	}
	ip := inst.GetNetworkInterfaces()[0].GetNetworkIP()

	runAs := func(user string, signer ssh.Signer, cmd string, stdin []byte) (string, error) {
		sshAddr := vmSSHAddress(t, addr, p, zone, vm)
		var out string
		var err error
		deadline := time.Now().Add(3 * time.Minute)
		for {
			var c *ssh.Client
			c, err = ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				var s *ssh.Session
				if s, err = c.NewSession(); err == nil {
					if stdin != nil {
						s.Stdin = bytes.NewReader(stdin)
					}
					var b []byte
					b, err = s.CombinedOutput(cmd)
					out = string(b)
				}
				c.Close()
				if err == nil {
					return out, nil
				}
				if _, exit := err.(*ssh.ExitError); exit {
					return out, err
				}
			}
			if time.Now().After(deadline) {
				return out, err
			}
			time.Sleep(2 * time.Second)
		}
	}
	run := func(cmd string) string {
		t.Helper()
		out, err := runAs("tester", signer1, cmd, nil)
		if err != nil {
			t.Fatalf("ssh %q: %v: %s", cmd, err, out)
		}
		return out
	}

	// curl, as on Compute Engine.
	const hdr = "-H 'Metadata-Flavor: Google'"
	md := "http://metadata.google.internal/computeMetadata/v1/"
	for _, c := range []struct{ what, cmd, want string }{
		{"GCE_METADATA_HOST", `echo "$GCE_METADATA_HOST"`, "metadata.google.internal"},
		{"name by metadata.google.internal", "curl -s " + hdr + " " + md + "instance/name", vm},
		{"zone by 169.254.169.254", "curl -s " + hdr + " http://169.254.169.254/computeMetadata/v1/instance/zone", "/zones/" + zone},
		{"name by GCE_METADATA_HOST", `curl -s ` + hdr + ` "http://$GCE_METADATA_HOST/computeMetadata/v1/instance/name"`, vm},
		{"by the short name", "curl -s " + hdr + " http://metadata/computeMetadata/v1/instance/attributes/role", "web"},
		{"the network interface", "curl -s " + hdr + " " + md + "instance/network-interfaces/0/ip", ip},
		{"recursive", "curl -s " + hdr + " '" + md + "instance/?recursive=true'", `"networkInterfaces":[`},
		{"the account", "curl -s " + hdr + " " + md + "instance/service-accounts/", sa + "/"},
		{"a token", "curl -s " + hdr + " " + md + "instance/service-accounts/default/token | grep -o '\"token_type\":\"Bearer\"'", `"token_type":"Bearer"`},
		{"an ETag", "curl -s -D - -o /dev/null " + hdr + " " + md + "instance/id | tr -d '\\r' | grep -i '^etag: '", "tag: "},
		{"the probe answers without the header", "curl -s -D - -o /dev/null http://169.254.169.254/ | tr -d '\\r' | grep -i '^metadata-flavor'", "Google"},
		{"no Metadata-Flavor", "curl -s -o /dev/null -w '%{http_code}' " + md + "instance/name", "403"},
		{"X-Forwarded-For", "curl -s -o /dev/null -w '%{http_code}' " + hdr + " -H 'X-Forwarded-For: 10.0.0.1' " + md + "instance/name", "403"},
		{"a guest attribute", "curl -s -o /dev/null -w '%{http_code}' -X PUT --data ready " + hdr + " " + md + "instance/guest-attributes/app/state", "200"},
	} {
		if out := run(c.cmd); !strings.Contains(out, c.want) {
			t.Errorf("%s: %q, want %q", c.what, out, c.want)
		}
	}
	ga, err := insts.GetGuestAttributes(ctx, &computepb.GetGuestAttributesInstanceRequest{Project: p, Zone: zone, Instance: vm, QueryPath: proto.String("app/")})
	if err != nil {
		t.Fatal(err)
	}
	if items := ga.GetQueryValue().GetItems(); len(items) != 1 || items[0].GetKey() != "state" || items[0].GetValue() != "ready" {
		t.Errorf("getGuestAttributes: %v", ga)
	}
	ga, err = insts.GetGuestAttributes(ctx, &computepb.GetGuestAttributesInstanceRequest{Project: p, Zone: zone, Instance: vm, VariableKey: proto.String("app/state")})
	if err != nil || ga.GetVariableValue() != "ready" {
		t.Errorf("getGuestAttributes variableKey: %v %v", ga, err)
	}

	// Google's metadata package, in the guest.
	if out, err := runAs("tester", signer1, "cat > /tmp/vmmetadata && chmod +x /tmp/vmmetadata", bin); err != nil {
		t.Fatalf("copy the probe: %v: %s", err, out)
	}
	out := run("/tmp/vmmetadata")
	for _, want := range []string{"ongce=true", "project=" + p + "\n", "name=" + vm + "\n", "id=" + fmt.Sprint(inst.GetId()) + "\n",
		"zone=" + zone + "\n", "ip=" + ip + "\n", "email=" + sa + "\n", "role=web\n", "nic=" + ip + "\n", "token=Bearer expires_in=",
		"scopes=https://www.googleapis.com/auth/cloud-platform\n", "OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("the metadata package: no %q in %s", strings.TrimSpace(want), out)
		}
	}
	// Without GCE_METADATA_HOST, the package's own default names it too.
	if out := run("env -u GCE_METADATA_HOST /tmp/vmmetadata"); !strings.Contains(out, "name="+vm+"\n") || !strings.Contains(out, "OK") {
		t.Errorf("the metadata package's default host: %s", out)
	}
	// Subscribe (wait_for_change) sees a setMetadata, which also gives the
	// guest agent a second user's key.
	var wg sync.WaitGroup
	var watched string
	var watchErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		watched, watchErr = runAs("tester", signer1, "/tmp/vmmetadata watch", nil)
	}()
	time.Sleep(5 * time.Second)
	wait(insts.SetMetadata(ctx, &computepb.SetMetadataInstanceRequest{Project: p, Zone: zone, Instance: vm,
		MetadataResource: &computepb.Metadata{Fingerprint: inst.GetMetadata().Fingerprint, Items: []*computepb.Items{
			{Key: proto.String("role"), Value: proto.String("db")},
			{Key: proto.String("enable-guest-attributes"), Value: proto.String("TRUE")},
			{Key: proto.String("ssh-keys"), Value: proto.String(line1 + "\n" + line2)}}}}))
	wg.Wait()
	if watchErr != nil || !strings.Contains(watched, "watch1=web\n") || !strings.Contains(watched, "watch2=db\n") {
		t.Errorf("Subscribe: %v: %s", watchErr, watched)
	}
	if out, err := runAs("second", signer2, "id -un; sudo -n true && echo SUDO-OK", nil); err != nil || !strings.Contains(out, "second") || !strings.Contains(out, "SUDO-OK") {
		t.Errorf("the second user's pushed key: %v: %s", err, out)
	}
}
