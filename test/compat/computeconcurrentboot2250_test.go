//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// concurrentBootVMs is how many VMs TestComputeConcurrentVMBootsAnswerSSH2250
// inserts at once: a managed instance group of two booting on a loaded
// runner left the second VM's sshd unanswered (main run 45555303799).
const concurrentBootVMs = 3

// covers: compute.instances.insert, compute.instances.get, compute.instances.getSerialPortOutput, compute.instances.delete
//
// TestComputeConcurrentVMBootsAnswerSSH2250 (#2250), the official
// compute/apiv1 REST client: debian-12 VMs inserted at the same moment,
// each with ssh-keys as gcloud compute ssh adds them, each insert's
// operation DONE without an error and its instance RUNNING, and every one
// answering an SSH login, as a Compute Engine VM that is RUNNING boots.
// A failed one's serial console is in the failure.
func TestComputeConcurrentVMBootsAnswerSSH2250(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvCompute)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = insts.Close() })
	p, zone := h.Project(), "us-central1-a"
	sfx := time.Now().UnixNano() % 1e6

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"

	names := make([]string, concurrentBootVMs)
	for i := range names {
		names[i] = fmt.Sprintf("boot-%d-%d", sfx, i)
	}
	t.Cleanup(func() {
		c := context.Background()
		var wg sync.WaitGroup
		for _, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: n}); err == nil {
					_ = op.Wait(c)
				}
			}()
		}
		wg.Wait()
	})
	serial := func(n string) string {
		out, err := insts.GetSerialPortOutput(ctx, &computepb.GetSerialPortOutputInstanceRequest{Project: p, Zone: zone, Instance: n})
		if err != nil {
			return "(no serial port output: " + err.Error() + ")"
		}
		s := out.GetContents()
		if len(s) > 6000 {
			s = s[len(s)-6000:]
		}
		return s
	}

	errs := make([]error, len(names))
	var wg sync.WaitGroup
	start := time.Now()
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
				Name:        proto.String(n),
				MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
				Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
				NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
				Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
			}})
			if err == nil {
				err = op.Wait(ctx)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	t.Logf("%d concurrent inserts done in %s", len(names), time.Since(start).Round(time.Second))
	for i, n := range names {
		if errs[i] != nil {
			t.Errorf("insert %s: %v", n, errs[i])
			continue
		}
		inst, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: n})
		if err != nil || inst.GetStatus() != "RUNNING" {
			t.Errorf("%s: %s %v\n%s", n, inst.GetStatus(), err, serial(n))
		}
	}
	if t.Failed() {
		return
	}

	for _, n := range names {
		sshAddr := vmSSHAddress(t, addr, p, zone, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(3 * time.Minute)
			for {
				c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
					HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
				var out []byte
				if err == nil {
					var s *ssh.Session
					if s, err = c.NewSession(); err == nil {
						out, err = s.CombinedOutput("hostname")
					}
					c.Close()
				}
				if err == nil && strings.TrimSpace(string(out)) == n {
					return
				}
				if time.Now().After(deadline) {
					t.Errorf("ssh %s (%s): %v %q\n%s", n, sshAddr, err, out, serial(n))
					return
				}
				time.Sleep(2 * time.Second)
			}
		}()
	}
	wg.Wait()
}
