package awsprov_test

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
)

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// TestSessionManagerCreateTunnelDelete: an SSM-mode VM has a role with
// AmazonSSMManagedInstanceCore and nothing inbound; the tunnel waits for its
// agent and carries a connection to its port 22 through StartSession and
// the data channel; Delete removes the role and profile too.
func TestSessionManagerCreateTunnelDelete(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	f.SetSSMTarget(echoServer(t))
	sp := spec("ssm")
	sp.ConnectMode, sp.SourceCIDR = byoc.ConnectSSM, ""
	ev := &events{}
	m, err := p.Create(ctx(t), sp, ev.progress())
	if err != nil {
		t.Fatal(err)
	}
	if m.ConnectMode != byoc.ConnectSSM || m.IDs["role"] != "cloudburrow-ssm-ssm" || !ev.has("role", byoc.StepDone) {
		t.Fatalf("machine %+v", m)
	}
	var role, profile, inst bool
	for _, r := range f.Live() {
		switch r.Kind {
		case "security-group":
			if len(r.Ingress) != 0 {
				t.Errorf("SSM mode opened %v inbound", r.Ingress)
			}
		case "iam-role":
			role = r.Tags[byoc.TagKey] == "ssm" && len(r.Members) == 1 && r.Members[0] == awsfake.SSMCorePolicy
		case "instance-profile":
			profile = r.Tags[byoc.TagKey] == "ssm" && len(r.Members) == 1
		case "instance":
			inst = r.Profile == "cloudburrow-ssm-ssm"
		}
	}
	if !role || !profile || !inst {
		t.Fatalf("role %v, profile %v, instance launched with it %v: %+v", role, profile, inst, f.Live())
	}
	res, err := p.ListResources(ctx(t), "ssm", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range res {
		kinds = append(kinds, r.Type)
	}
	if !strings.Contains(strings.Join(kinds, " "), "iam:role") || !strings.Contains(strings.Join(kinds, " "), "iam:instance-profile") {
		t.Errorf("ListResources %v misses the role or profile", kinds)
	}

	tp, ok := p.(byoc.Tunneler)
	if !ok {
		t.Fatal("the AWS provider offers no tunnel")
	}
	tun, err := tp.OpenTunnel(ctx(t), m, 22, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tun.Port())))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := c.Write([]byte("ping over ssm")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("ping over ssm"))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping over ssm" {
		t.Fatalf("echo through the tunnel = %q %v", got, err)
	}
	c.Close()
	if f.Count("ssm:StartSession") != 1 || f.Count("ssmmessages:OpenDataChannel") != 1 {
		t.Errorf("calls %v", f.Calls())
	}
	_ = tun.Close()
	if started, ended := f.SSMSessions(); started != 1 || ended != 1 {
		t.Errorf("sessions started %d, terminated %d", started, ended)
	}

	// A stopped VM's agent is not connected: the reason is plain.
	if _, err := p.Stop(ctx(t), m); err != nil {
		t.Fatal(err)
	}
	tun2, err := tp.OpenTunnel(ctx(t), m, 22, nil)
	if err == nil {
		tun2.Close()
		t.Fatal("a tunnel opened to a stopped VM")
	} else if !strings.Contains(err.Error(), "SSM agent") {
		t.Errorf("the refusal says %v", err)
	}

	if err := p.Delete(ctx(t), "ssm", "us-east-1", nil); err != nil {
		t.Fatal(err)
	}
	if left := f.Live(); len(left) != 0 {
		t.Errorf("Delete left %+v", left)
	}
}

func TestSessionManagerRoleFailureRollsBack(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	f.Deny("iam:AddRoleToInstanceProfile")
	sp := spec("ssmfail")
	sp.ConnectMode, sp.SourceCIDR = byoc.ConnectSSM, ""
	if _, err := p.Create(ctx(t), sp, nil); err == nil || !strings.Contains(err.Error(), "attach the role to the VM") {
		t.Fatalf("Create = %v, want the missing permission named", err)
	}
	if left := f.Live(); len(left) != 0 {
		t.Errorf("the failed create left %+v", left)
	}
}

func TestUpdateSourceAddressMovesTheSSHRule(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	m, err := p.Create(ctx(t), spec("moving"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.UpdateSourceAddress(ctx(t), m, "198.51.100.200/32")
	if err != nil || got.SourceCIDR != "198.51.100.200/32" {
		t.Fatalf("UpdateSourceAddress = %+v %v", got, err)
	}
	for _, r := range f.Live() {
		if r.Kind == "security-group" && (len(r.Ingress) != 1 || r.Ingress[0] != "tcp 22-22 198.51.100.200/32") {
			t.Errorf("ingress %v, want only the new address", r.Ingress)
		}
	}
	// Again with the same address: nothing changes.
	if _, err := p.UpdateSourceAddress(ctx(t), got, "198.51.100.200/32"); err != nil {
		t.Fatal(err)
	}
	if f.Count("ec2:RevokeSecurityGroupIngress") != 1 {
		t.Errorf("revokes %d", f.Count("ec2:RevokeSecurityGroupIngress"))
	}
}
