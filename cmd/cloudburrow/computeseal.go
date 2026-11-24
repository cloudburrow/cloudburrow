package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// A disk with a customer-managed or customer-supplied key is sealed at
// rest (#2206, compute's diskseal.go): its claim holds only
// compute.SealedFile while no running instance has it open. The guest's
// files are read and written by a helper pod of the Debian image (tar,
// already in it) over kubectl exec, and encrypted and decrypted here, in
// this process, with Go's standard library: the data key never reaches the
// cluster, and the plaintext never touches this machine's disk.

const sealedTmp = compute.SealedFile + ".tmp"

// helperPod starts a pod with the claim mounted at /d, waits for it and
// returns its name and a function that deletes it.
func (b *computeVMs) helperPod(ctx context.Context, claim string) (string, func(), error) {
	ref, err := b.ensureImage(ctx, compute.PublicImages[0])
	if err != nil {
		return "", nil, err
	}
	name := "seal-" + claim
	if len(name) > 63 {
		name = name[:63]
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(), "labels": map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute"}},
		"spec": map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "terminationGracePeriodSeconds": 0,
			"containers": []any{map[string]any{"name": "seal", "image": ref, "imagePullPolicy": "Never",
				"command":      []string{"/bin/sh", "-c", "sleep 3600"},
				"volumeMounts": []any{map[string]any{"name": "d", "mountPath": "/d"}}}},
			"volumes": []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim}}}}}
	m, _ := json.Marshal(pod)
	_, _ = b.kubectl(ctx, "delete", "pod", name, "--ignore-not-found", "--wait=true")
	if err := b.installer.Apply(ctx, string(m)); err != nil {
		return "", nil, err
	}
	done := func() {
		_, _ = b.kubectl(context.Background(), "delete", "pod", name, "--ignore-not-found", "--wait=false")
	}
	if _, err := b.kubectl(ctx, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=5m"); err != nil {
		done()
		return "", nil, fmt.Errorf("open %s: %w", claim, err)
	}
	return name, done, nil
}

// podExec runs script in the helper pod, its stdin and stdout streamed.
func (b *computeVMs) podExec(ctx context.Context, pod string, stdin io.Reader, stdout io.Writer, script string) error {
	r := k8s.NewWith(k8s.Subprocess{}, b.cfg.KubeconfigPath(), "", b.ns())
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, pod, "--", "/bin/sh", "-c", script)
	var stderr bytes.Buffer
	if err := r.Pipe(ctx, stdin, stdout, &stderr, args...); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(vmTail(stderr.String(), 3)))
	}
	return nil
}

func (b *computeVMs) sealed(ctx context.Context, pod string) (bool, error) {
	var out bytes.Buffer
	if err := b.podExec(ctx, pod, nil, &out, "if [ -e /d/"+compute.SealedFile+" ]; then echo yes; else echo no; fi"); err != nil {
		return false, err
	}
	return strings.TrimSpace(out.String()) == "yes", nil
}

// sealIn seals /d with key: its files, as a tar stream, become SealedFile,
// and the files are removed.
func (b *computeVMs) sealIn(ctx context.Context, pod string, key []byte) error {
	if ok, err := b.sealed(ctx, pod); err != nil || ok {
		return err
	}
	tarR, tarW := io.Pipe()
	sealR, sealW := io.Pipe()
	errc := make(chan error, 2)
	go func() {
		err := b.podExec(ctx, pod, nil, tarW,
			"cd /d && tar --numeric-owner --exclude=./"+compute.SealedFile+"* -cpf - .")
		_ = tarW.CloseWithError(err)
		errc <- err
	}()
	go func() {
		err := compute.Seal(sealW, tarR, key)
		_ = tarR.CloseWithError(err)
		_ = sealW.CloseWithError(err)
		errc <- err
	}()
	werr := b.podExec(ctx, pod, sealR, nil, "cat > /d/"+sealedTmp+" && sync")
	_ = sealR.CloseWithError(werr)
	for range 2 {
		if err := <-errc; err != nil && werr == nil {
			werr = err
		}
	}
	if werr != nil {
		_ = b.podExec(context.Background(), pod, nil, nil, "rm -f /d/"+sealedTmp)
		return fmt.Errorf("seal the disk: %w", werr)
	}
	return b.podExec(ctx, pod, nil, nil, "cd /d && find . -mindepth 1 -maxdepth 1 ! -name "+sealedTmp+
		" -exec rm -rf {} + && mv "+sealedTmp+" "+compute.SealedFile+" && sync")
}

// unsealIn opens /d's SealedFile with key into its files.
func (b *computeVMs) unsealIn(ctx context.Context, pod string, key []byte) error {
	if ok, err := b.sealed(ctx, pod); err != nil || !ok {
		return err
	}
	if key == nil {
		return fmt.Errorf("the disk is encrypted, and its data key was not given")
	}
	catR, catW := io.Pipe()
	openR, openW := io.Pipe()
	errc := make(chan error, 2)
	go func() {
		err := b.podExec(ctx, pod, nil, catW, "cat /d/"+compute.SealedFile)
		_ = catW.CloseWithError(err)
		errc <- err
	}()
	go func() {
		err := compute.Open(openW, catR, key)
		_ = catR.CloseWithError(err)
		_ = openW.CloseWithError(err)
		errc <- err
	}()
	werr := b.podExec(ctx, pod, openR, nil, "tar --numeric-owner -xpf - -C /d")
	_ = openR.CloseWithError(werr)
	for range 2 {
		if err := <-errc; err != nil {
			werr = err // the decryption's error says more than tar's
		}
	}
	if werr != nil {
		// Leave the sealed contents only.
		_ = b.podExec(context.Background(), pod, nil, nil, "cd /d && find . -mindepth 1 -maxdepth 1 ! -name "+compute.SealedFile+" -exec rm -rf {} +")
		return fmt.Errorf("open the encrypted disk: %w", werr)
	}
	return b.podExec(ctx, pod, nil, nil, "rm -f /d/"+compute.SealedFile+" && sync")
}

// rekey leaves a claim whose files came from a source sealed with from
// (or in the clear) sealed with to (or in the clear).
func (b *computeVMs) rekey(ctx context.Context, claim string, from, to []byte) error {
	if from == nil && to == nil {
		return nil
	}
	pod, done, err := b.helperPod(ctx, claim)
	if err != nil {
		return err
	}
	defer done()
	if from != nil && to != nil && bytes.Equal(from, to) {
		if ok, err := b.sealed(ctx, pod); err != nil || ok {
			return err
		}
	}
	if err := b.unsealIn(ctx, pod, from); err != nil {
		return err
	}
	if to != nil {
		return b.sealIn(ctx, pod, to)
	}
	return nil
}

func (b *computeVMs) SealDisk(ctx context.Context, d compute.DiskSpec) error {
	if d.DataKey == nil || d.Open {
		return nil
	}
	pod, done, err := b.helperPod(ctx, d.Key)
	if err != nil {
		return err
	}
	defer done()
	return b.sealIn(ctx, pod, d.DataKey)
}

func (b *computeVMs) UnsealDisk(ctx context.Context, d compute.DiskSpec) error {
	if d.DataKey == nil {
		return nil
	}
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), d.Key, d.SizeGb, "disk")); err != nil {
		return err
	}
	pod, done, err := b.helperPod(ctx, d.Key)
	if err != nil {
		return err
	}
	defer done()
	return b.unsealIn(ctx, pod, d.DataKey)
}
