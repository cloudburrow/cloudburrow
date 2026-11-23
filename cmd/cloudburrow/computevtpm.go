package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// A Shielded VM's vTPM (#2179): swtpm (BSD-3-Clause, the guest image's
// distribution package, run in a container of its own beside the guest,
// never linked into CloudBurrow) on the instance's vTPM claim, its command
// socket shared with the guest at /run/vtpm/tpm.sock, which the guest's
// cloudburrow-vtpm unit names /dev/tpm0 and /dev/tpmrm0 (a Unix socket
// TPM, as go-tpm and tpm2-tss's swtpm TCTI open it). Compute Engine's own
// provisioning and measurements (internal/service/compute/vtpm.go) reach
// it through a kubectl exec of the vTPM container's relay
// (usr/lib/cloudburrow/tpm-relay), one TPM command at a time.

const (
	vtpmContainer = "vtpm"
	vtpmSocketDir = "/run/vtpm"
)

func vtpmClaim(vm compute.VMSpec) string { return vm.Key + "-vtpm" }

// vtpmPod adds the vTPM container, its claim and the shared socket
// directory to a Shielded VM's pod spec.
func vtpmPod(vm compute.VMSpec, ref string, spec map[string]any, mounts []any) []any {
	if !vm.VTPM {
		return mounts
	}
	script := "set -e; mkdir -p /vtpm/state " + vtpmSocketDir + "; rm -f " + vtpmSocketDir + "/tpm.sock " + vtpmSocketDir + "/ctrl.sock; " +
		"exec swtpm socket --tpm2 --tpmstate dir=/vtpm/state --server type=unixio,path=" + vtpmSocketDir + "/tpm.sock,mode=0660 " +
		"--ctrl type=unixio,path=" + vtpmSocketDir + "/ctrl.sock --flags not-need-init,startup-clear"
	c := map[string]any{"name": vtpmContainer, "image": ref, "imagePullPolicy": "Never",
		"command": []string{"/bin/sh", "-c", script},
		"volumeMounts": []any{map[string]any{"name": "vtpm-state", "mountPath": "/vtpm"},
			map[string]any{"name": "vtpm-run", "mountPath": vtpmSocketDir}},
		"readinessProbe": map[string]any{"exec": map[string]any{"command": []string{"test", "-S", vtpmSocketDir + "/tpm.sock"}},
			"periodSeconds": 1},
		"resources": map[string]any{"limits": map[string]string{"cpu": "500m", "memory": "64Mi"},
			"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}}}
	// A native sidecar (an init container that keeps running): the vTPM
	// is there before the guest boots, as it is from power on.
	c["restartPolicy"] = "Always"
	c["startupProbe"] = map[string]any{"exec": map[string]any{"command": []string{"test", "-S", vtpmSocketDir + "/tpm.sock"}},
		"periodSeconds": 1, "failureThreshold": 60}
	inits, _ := spec["initContainers"].([]any)
	spec["initContainers"] = append(inits, c)
	vols, _ := spec["volumes"].([]any)
	spec["volumes"] = append(vols,
		map[string]any{"name": "vtpm-state", "persistentVolumeClaim": map[string]any{"claimName": vtpmClaim(vm)}},
		map[string]any{"name": "vtpm-run", "emptyDir": map[string]any{"medium": "Memory"}})
	return append(mounts, map[string]any{"name": "vtpm-run", "mountPath": vtpmSocketDir})
}

// ensureVTPMClaim makes the instance's vTPM claim when it has a vTPM.
func (b *computeVMs) ensureVTPMClaim(ctx context.Context, vm compute.VMSpec) error {
	if !vm.VTPM {
		return nil
	}
	return b.installer.Apply(ctx, pvcManifest(b.ns(), vtpmClaim(vm), 1, "vtpm"))
}

// DeleteTPM implements compute.VMTPM: the instance's vTPM state goes with
// the instance.
func (b *computeVMs) DeleteTPM(ctx context.Context, vm compute.VMSpec) error {
	_, err := b.kubectl(ctx, "delete", "pvc", vtpmClaim(vm), "--ignore-not-found", "--wait=true", "--timeout=2m")
	return err
}

// OpenTPM implements compute.VMTPM.
func (b *computeVMs) OpenTPM(ctx context.Context, vm compute.VMSpec) (io.ReadWriteCloser, error) {
	cmd := exec.CommandContext(b.bg, "kubectl", "--kubeconfig", b.cfg.KubeconfigPath(), "-n", b.ns(), "exec", "-i", vm.Key,
		"-c", vtpmContainer, "--", "perl", "/usr/lib/cloudburrow/tpm-relay", vtpmSocketDir+"/tpm.sock")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &tpmPipe{cmd: cmd, in: in, out: out}, nil
}

// tpmPipe is a TPM command channel over the relay's stdin and stdout: each
// Write a command, each Read its whole response.
type tpmPipe struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.Reader
	resp []byte
}

func (p *tpmPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.in.Write(b); err != nil {
		return 0, err
	}
	hdr := make([]byte, 10)
	done := make(chan error, 1)
	go func() {
		if _, err := io.ReadFull(p.out, hdr); err != nil {
			done <- err
			return
		}
		n := binary.BigEndian.Uint32(hdr[2:6])
		if n < 10 || n > 1<<16 {
			done <- fmt.Errorf("vTPM response of %d bytes", n)
			return
		}
		rest := make([]byte, n-10)
		if _, err := io.ReadFull(p.out, rest); err != nil {
			done <- err
			return
		}
		p.resp = append(hdr, rest...)
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			return 0, err
		}
	case <-time.After(2 * time.Minute):
		return 0, fmt.Errorf("the vTPM did not answer")
	}
	return len(b), nil
}

func (p *tpmPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resp == nil {
		return 0, io.EOF
	}
	n := copy(b, p.resp)
	p.resp = p.resp[n:]
	if len(p.resp) == 0 {
		p.resp = nil
	}
	return n, nil
}

func (p *tpmPipe) Close() error {
	_ = p.in.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	return nil
}

// integrityEvent writes a Shielded VM's integrity monitoring event to the
// instance's Cloud Logging, as Compute Engine writes it: the
// compute.googleapis.com/shielded_vm_integrity log of the instance's
// gce_instance resource (compute/shielded-vm/docs/integrity-monitoring).
func (s *computeService) integrityEvent(ctx context.Context, project, zone, instanceID string, payload map[string]any) {
	if s.natLogs == nil {
		return
	}
	st, err := structpb.NewStruct(payload)
	if err != nil {
		return
	}
	sev := ltype.LogSeverity_NOTICE
	for _, k := range []string{"earlyBootReportEvent", "lateBootReportEvent"} {
		if ev, ok := payload[k].(map[string]any); ok && ev["policyEvaluationPassed"] != true {
			sev = ltype.LogSeverity_ERROR
		}
	}
	_ = s.natLogs(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
		LogName:  "projects/" + project + "/logs/compute.googleapis.com%2Fshielded_vm_integrity",
		Resource: &monitoredres.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"project_id": project, "zone": zone, "instance_id": instanceID}},
		Severity: sev,
		Payload:  &loggingpb.LogEntry_JsonPayload{JsonPayload: st},
	}}})
}
