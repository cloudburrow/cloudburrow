//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// shieldedVM is a running Debian VM a test reaches over SSH.
type shieldedVM struct {
	t     *testing.T
	insts *computeapi.InstancesClient
	ctx   context.Context
	addr  string
	p     string
	zone  string
	name  string
	sign  ssh.Signer
}

func newShieldedVM(t *testing.T, h *Harness, ctx context.Context, insts *computeapi.InstancesClient, prefix string, items []*computepb.Items, sa *computepb.ServiceAccount, shielded *computepb.ShieldedInstanceConfig) *shieldedVM {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	line := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " tester"
	v := &shieldedVM{t: t, insts: insts, ctx: ctx, addr: h.Endpoint(EnvCompute), p: h.Project(), zone: "us-central1-a",
		name: fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e6), sign: signer}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: v.p, Zone: v.zone, Instance: v.name}); err == nil {
			_ = op.Wait(c)
		}
	})
	items = append(items, &computepb.Items{Key: proto.String("ssh-keys"), Value: proto.String(line)})
	op, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: v.p, Zone: v.zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(v.name),
		MachineType: proto.String("zones/" + v.zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces:      []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:               &computepb.Metadata{Items: items},
		ServiceAccounts:        []*computepb.ServiceAccount{sa},
		ShieldedInstanceConfig: shielded,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}

// run runs cmd over SSH (stdin, when set, its input), retrying while the
// guest boots.
func (v *shieldedVM) run(cmd string, stdin []byte) (string, error) {
	sshAddr := vmSSHAddress(v.t, v.addr, v.p, v.zone, v.name)
	var out string
	var err error
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var c *ssh.Client
		c, err = ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(v.sign)},
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

func (v *shieldedVM) must(cmd string) string {
	v.t.Helper()
	out, err := v.run(cmd, nil)
	if err != nil {
		v.t.Fatalf("ssh %q: %v: %s", cmd, err, out)
	}
	return out
}

// TestShieldedVMHTTPSMetadataServer2179: a Shielded VM (#2179) has a vTPM
// of its own (swtpm) and, with disable-https-mds-setup FALSE, Google's
// guest agent fetches its HTTPS metadata server credentials
// (instance/credentials/mds-client-certificate, sealed to the vTPM's
// endorsement key), writing /run/google-mds-mtls/root.crt and client.key
// from the root certificate in its UEFI variable, as Google documents
// (compute/docs/metadata/overview#https-mds). curl reads the instance's
// metadata from https://metadata.google.internal with that client
// certificate, and is refused without one; Google's Go metadata package
// reads it over HTTPS too; getShieldedInstanceIdentity, through Google's
// compute client, returns the vTPM's endorsement and signing keys, whose
// certificates certify those keys; guest attributes are held to their
// size and rate limits.
func TestShieldedVMHTTPSMetadataServer2179(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)

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
	sa := "vm-shielded@" + h.Project() + ".iam.gserviceaccount.com"
	v := newShieldedVM(t, h, ctx, insts, "svm", []*computepb.Items{
		{Key: proto.String("disable-https-mds-setup"), Value: proto.String("FALSE")},
		{Key: proto.String("enable-guest-attributes"), Value: proto.String("TRUE")}},
		&computepb.ServiceAccount{Email: proto.String(sa), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}},
		&computepb.ShieldedInstanceConfig{EnableVtpm: proto.Bool(true), EnableIntegrityMonitoring: proto.Bool(true)})

	inst, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: v.p, Zone: v.zone, Instance: v.name})
	if err != nil {
		t.Fatal(err)
	}
	if sc := inst.GetShieldedInstanceConfig(); !sc.GetEnableVtpm() || !sc.GetEnableIntegrityMonitoring() || sc.GetEnableSecureBoot() {
		t.Errorf("shieldedInstanceConfig: %v", sc)
	}

	// getShieldedInstanceIdentity: the vTPM's keys, certified.
	id, err := insts.GetShieldedInstanceIdentity(ctx, &computepb.GetShieldedInstanceIdentityInstanceRequest{Project: v.p, Zone: v.zone, Instance: v.name})
	if err != nil {
		t.Fatal(err)
	}
	for what, e := range map[string]*computepb.ShieldedInstanceIdentityEntry{"encryptionKey": id.GetEncryptionKey(), "signingKey": id.GetSigningKey(),
		"eccP256EncryptionKey": id.GetEccP256EncryptionKey(), "eccP256SigningKey": id.GetEccP256SigningKey()} {
		pb, _ := pem.Decode([]byte(e.GetEkPub()))
		cb, _ := pem.Decode([]byte(e.GetEkCert()))
		if pb == nil || cb == nil {
			t.Errorf("%s: %v", what, e)
			continue
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			t.Errorf("%s certificate: %v", what, err)
			continue
		}
		pubDER, _ := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if !bytes.Equal(pubDER, pb.Bytes) {
			t.Errorf("%s: the certificate does not certify ekPub", what)
		}
	}

	// The guest: the vTPM, the agent's credentials, and curl over HTTPS.
	const hdr = "-H 'Metadata-Flavor: Google'"
	creds := "--cacert /run/google-mds-mtls/root.crt --cert /run/google-mds-mtls/client.key"
	if out := v.must("for i in $(seq 90); do [ -s /run/google-mds-mtls/client.key ] && break; sleep 2; done; ls -l /dev/tpm0 /run/google-mds-mtls/; true"); !strings.Contains(out, "client.key") || !strings.Contains(out, "root.crt") {
		// The agent's debug log: restarted with log_level 4, its
		// setup is logged again.
		diag, _ := v.run(`sudo sed -i 's/^\[Core\]$/[Core]\nlog_level = 4\nlog_verbosity = 3/' /etc/default/instance_configs.cfg; `+
			`sudo systemctl restart google-guest-agent-manager; sleep 60; ls -l /run/google-mds-mtls/ 2>&1; `+
			`sudo journalctl -b --no-pager -u google-guest-agent-manager 2>&1 | grep -v -i 'acs client\|metric\|telemetry\|mwlid' | tail -120`, nil)
		t.Fatalf("Google's guest agent wrote no HTTPS metadata server credentials: %s\n%s", out, diag)
	}
	for _, c := range []struct{ what, cmd, want string }{
		{"name over HTTPS", "curl -s " + creds + " " + hdr + " https://metadata.google.internal/computeMetadata/v1/instance/name", v.name},
		{"by 169.254.169.254 over HTTPS", "curl -s " + creds + " " + hdr + " https://169.254.169.254/computeMetadata/v1/instance/zone", "/zones/" + v.zone},
		{"a token over HTTPS", "curl -s " + creds + " " + hdr + " https://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token | grep -o '\"token_type\":\"Bearer\"'", `"token_type":"Bearer"`},
		{"no client certificate", "curl -s -o /dev/null -w '%{http_code}' --cacert /run/google-mds-mtls/root.crt " + hdr + " https://metadata.google.internal/computeMetadata/v1/instance/name; echo \" rc=$?\"", "000"},
		{"the server is the instance's", "curl -s -o /dev/null -w '%{http_code}' " + hdr + " https://metadata.google.internal/computeMetadata/v1/instance/name; echo \" rc=$?\"", "rc=60"},
		{"HTTP still answers", "curl -s " + hdr + " http://metadata.google.internal/computeMetadata/v1/instance/name", v.name},
	} {
		if out := v.must(c.cmd); !strings.Contains(out, c.want) {
			t.Errorf("%s: %q, want %q", c.what, out, c.want)
		}
	}
	// Google's Go metadata package, over HTTPS.
	if out, err := v.run("cat > /tmp/vmmetadata && chmod +x /tmp/vmmetadata", bin); err != nil {
		t.Fatalf("copy the probe: %v: %s", err, out)
	}
	out := v.must("/tmp/vmmetadata https")
	for _, want := range []string{"transport=https", "name=" + v.name + "\n", "email=" + sa + "\n", "token=Bearer expires_in=", "OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("the metadata package over HTTPS: no %q in %s", strings.TrimSpace(want), out)
		}
	}

	// Guest attributes: 256 KiB values, 128-byte keys, 3 writes a second.
	md := "http://metadata.google.internal/computeMetadata/v1/instance/guest-attributes/"
	if out := v.must("head -c 262145 /dev/zero | tr '\\0' a > /tmp/big; curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @/tmp/big " + hdr + " " + md + "app/big"); out != "400" {
		t.Errorf("a value over 256 KiB: %s, want 400", out)
	}
	if out := v.must("curl -s -o /dev/null -w '%{http_code}' -X PUT --data x " + hdr + " " + md + "app/" + strings.Repeat("k", 129)); out != "400" {
		t.Errorf("a key over 128 bytes: %s, want 400", out)
	}
	burst := v.must("sleep 61; for i in 1 2 3 4; do curl -s -o /dev/null -w '%{http_code} ' -X PUT --data $i " + hdr + " " + md + "app/n$i; done")
	if !strings.HasPrefix(burst, "200 200 200 429") {
		t.Errorf("a burst of 4 writes: %q, want three 200 then 429", burst)
	}

	// Integrity monitoring's policy, and the config, which needs the VM
	// stopped.
	op, err := insts.SetShieldedInstanceIntegrityPolicy(ctx, &computepb.SetShieldedInstanceIntegrityPolicyInstanceRequest{Project: v.p, Zone: v.zone, Instance: v.name,
		ShieldedInstanceIntegrityPolicyResource: &computepb.ShieldedInstanceIntegrityPolicy{UpdateAutoLearnPolicy: proto.Bool(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := insts.UpdateShieldedInstanceConfig(ctx, &computepb.UpdateShieldedInstanceConfigInstanceRequest{Project: v.p, Zone: v.zone, Instance: v.name,
		ShieldedInstanceConfigResource: &computepb.ShieldedInstanceConfig{EnableSecureBoot: proto.Bool(true)}}); err == nil {
		t.Errorf("updateShieldedInstanceConfig on a running VM succeeded")
	}
}

// TestVMAccessScopesLimitTokens2179: with IAM enforcement on, a VM's
// token carries the instance's access scopes (#2179): a VM whose only
// scope is logging.write may write log entries (its scope covers it; IAM
// then checks the account's roles) but not read them, which Google
// refuses with 403 ACCESS_TOKEN_SCOPE_INSUFFICIENT, and instance/
// service-accounts/default/scopes lists that one scope.
func TestVMAccessScopesLimitTokens2179(t *testing.T) {
	requireIAMEnforcement(t)
	h := New(t)
	opts := computeOpts(t, h)
	h.Endpoint(EnvLogging)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	sa := "vm-scoped@" + h.Project() + ".iam.gserviceaccount.com"
	v := newShieldedVM(t, h, ctx, insts, "scoped", nil,
		&computepb.ServiceAccount{Email: proto.String(sa), Scopes: []string{"https://www.googleapis.com/auth/logging.write"}}, nil)
	const hdr = "-H 'Metadata-Flavor: Google'"
	md := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/"
	if out := v.must("curl -s " + hdr + " " + md + "scopes"); strings.TrimSpace(out) != "https://www.googleapis.com/auth/logging.write" {
		t.Errorf("scopes: %q", out)
	}
	// The token stays in the guest's shell: only the answers are printed.
	call := func(path, body string) string {
		return v.must(`L=$(sudo cat /proc/1/environ | tr '\0' '\n' | sed -n 's/^CLOUDBURROW_LOGGING_HOST=//p'); ` +
			`T=$(curl -s ` + hdr + ` ` + md + `token | sed 's/.*"access_token":"\([^"]*\)".*/\1/'); ` +
			`curl -s -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d '` + body + `' "http://$L` + path + `"`)
	}
	read := call("/v2/entries:list", `{"resourceNames":["projects/`+h.Project()+`"]}`)
	if !strings.Contains(read, "ACCESS_TOKEN_SCOPE_INSUFFICIENT") || !strings.Contains(read, "insufficient authentication scopes") || !strings.Contains(read, "403") {
		t.Errorf("entries.list with logging.write only: %s", read)
	}
	write := call("/v2/entries:write", `{"logName":"projects/`+h.Project()+`/logs/scoped","resource":{"type":"global"},"entries":[{"textPayload":"x"}]}`)
	if strings.Contains(write, "ACCESS_TOKEN_SCOPE_INSUFFICIENT") {
		t.Errorf("entries.write with logging.write was refused for its scope: %s", write)
	}
}
