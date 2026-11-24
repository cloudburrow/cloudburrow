package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Image import from a Cloud Storage file (#2139): the .tar.gz's disk.raw is
// unpacked into the image's claim by a privileged pod of the Debian image,
// its Linux root partition (found here from the partition table: GPT or
// MBR, or a whole-disk file system) is loop-mounted read-only, and its
// /etc, /home, /opt, /root, /srv, /usr and /var are copied into the claim,
// the boot-disk layout every disk here has. A disk made from the image is
// a copy of that claim, so an instance booted from it runs the imported
// system's files under the container of its distribution (os-release ID).
// The kernel, boot loader and other partitions are not used: the
// container shares the host's kernel.

var _ compute.RawImporter = (*computeVMs)(nil)

// rawHeadBytes is what partitionOffset reads: the MBR, the GPT header and
// 128 entries of 128 bytes.
const rawHeadBytes = 512 * 34

// Linux partition type GUIDs (UEFI spec; systemd's Discoverable Partitions).
var linuxPartitionTypes = map[string]bool{
	"0fc63daf-8483-4772-8e79-3d69d8477de4": true, // Linux filesystem
	"4f68bce3-e8cd-4db1-96e7-fbcaf984b709": true, // Linux root (x86-64)
	"b921b045-1df0-41c3-af44-4c6f280d3fae": true, // Linux root (AArch64)
}

func gptGUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x", binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]), b[8:10], b[10:16])
}

// partitionOffset is the byte offset of a raw disk's Linux root file system:
// the largest Linux partition of its GPT or MBR, or 0 for a disk that is a
// file system itself (ext2/3/4 or XFS).
func partitionOffset(head []byte) (int64, error) {
	noLinux := errors.New("the disk.raw has no Linux partition")
	if len(head) >= 1024 && head[510] == 0x55 && head[511] == 0xaa {
		gpt := false
		for i := 0; i < 4; i++ {
			if head[446+16*i+4] == 0xee {
				gpt = true
			}
		}
		var best, bestLen int64 = -1, 0
		if gpt {
			if string(head[512:520]) != "EFI PART" {
				return 0, errors.New("the disk.raw's GPT header is missing")
			}
			lba := int64(binary.LittleEndian.Uint64(head[512+72:]))
			n := int64(binary.LittleEndian.Uint32(head[512+80:]))
			size := int64(binary.LittleEndian.Uint32(head[512+84:]))
			if size < 128 {
				return 0, errors.New("the disk.raw's GPT header is malformed")
			}
			for i := int64(0); i < n; i++ {
				off := lba*512 + i*size
				if off+size > int64(len(head)) {
					break
				}
				e := head[off : off+size]
				if !linuxPartitionTypes[gptGUID(e[0:16])] {
					continue
				}
				first, last := int64(binary.LittleEndian.Uint64(e[32:])), int64(binary.LittleEndian.Uint64(e[40:]))
				if last-first+1 > bestLen {
					best, bestLen = first*512, last-first+1
				}
			}
		} else {
			for i := 0; i < 4; i++ {
				e := head[446+16*i : 446+16*i+16]
				if e[4] != 0x83 {
					continue
				}
				first, n := int64(binary.LittleEndian.Uint32(e[8:])), int64(binary.LittleEndian.Uint32(e[12:]))
				if n > bestLen {
					best, bestLen = first*512, n
				}
			}
		}
		if best < 0 {
			return 0, noLinux
		}
		return best, nil
	}
	if len(head) >= 1082 && binary.LittleEndian.Uint16(head[1024+56:]) == 0xef53 {
		return 0, nil
	}
	if len(head) >= 4 && string(head[:4]) == "XFSB" {
		return 0, nil
	}
	return 0, noLinux
}

// podExec runs a command in a pod of the namespace with stdin, returning
// its standard output.
func (b *computeVMs) podExec(ctx context.Context, pod string, stdin io.Reader, script string) ([]byte, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	var out, errb bytes.Buffer
	if err := k8s.New(b.cfg.KubeconfigPath(), "", b.ns()).Pipe(ctx, stdin, &out, &errb, append(args, pod, "--", "/bin/sh", "-c", script)...); err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(vmTail(errb.String(), 5)))
	}
	return out.Bytes(), nil
}

func (b *computeVMs) ImportRaw(ctx context.Context, key string, sizeGb int64, open func(context.Context) (io.ReadCloser, error)) (string, error) {
	ref, err := b.ensureImage(ctx, compute.PublicImages[0])
	if err != nil {
		return "", err
	}
	// The claim holds the unpacked disk.raw while it is read: room for both.
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), key, 2*sizeGb, "snapshot")); err != nil {
		return "", err
	}
	name := "import-" + key
	if len(name) > 63 {
		name = name[:63]
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(), "labels": map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute"}},
		"spec": map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false,
			"containers": []any{map[string]any{"name": "import", "image": ref, "imagePullPolicy": "Never",
				// The loop mount needs it, as the instances' pods have it.
				"securityContext": map[string]any{"privileged": true},
				"command":         []string{"/bin/sh", "-c", "sleep 7200"},
				"volumeMounts":    []any{map[string]any{"name": "dst", "mountPath": "/dst"}}}},
			"volumes": []any{map[string]any{"name": "dst", "persistentVolumeClaim": map[string]any{"claimName": key}}}}}
	m, _ := json.Marshal(pod)
	_, _ = b.kubectl(ctx, "delete", "pod", name, "--ignore-not-found", "--wait=true")
	if err := b.installer.Apply(ctx, string(m)); err != nil {
		return "", err
	}
	defer func() {
		_, _ = b.kubectl(context.Background(), "delete", "pod", name, "--ignore-not-found", "--wait=false")
	}()
	if _, err := b.kubectl(ctx, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=5m"); err != nil {
		return "", fmt.Errorf("start the import pod: %w", err)
	}
	r, err := open(ctx)
	if err != nil {
		return "", err
	}
	_, err = b.podExec(ctx, name, r, "mkdir -p /dst/.import && tar -xzf - -C /dst/.import disk.raw")
	_ = r.Close()
	if err != nil {
		return "", fmt.Errorf("unpack disk.raw: %w", err)
	}
	head, err := b.podExec(ctx, name, nil, "head -c "+strconv.Itoa(rawHeadBytes)+" /dst/.import/disk.raw")
	if err != nil {
		return "", fmt.Errorf("read disk.raw: %w", err)
	}
	off, err := partitionOffset(head)
	if err != nil {
		return "", err
	}
	script := "set -e; mkdir -p /mnt/root; mount -o ro,loop,offset=" + strconv.FormatInt(off, 10) + " /dst/.import/disk.raw /mnt/root; " +
		"if [ ! -d /mnt/root/usr ] || [ ! -d /mnt/root/etc ]; then umount /mnt/root; echo 'the disk.raw has no Linux root file system (/usr and /etc)' >&2; exit 3; fi; " +
		"for d in " + strings.Join(rootDirs, " ") + "; do if [ -d /mnt/root/$d ]; then mkdir -p /dst/$d && cp -a /mnt/root/$d/. /dst/$d/; fi; done; " +
		"cat /mnt/root/usr/lib/os-release 2>/dev/null || cat /mnt/root/etc/os-release 2>/dev/null || true; " +
		"umount /mnt/root; rm -f /dst/.import/disk.raw; rmdir /dst/.import; touch /dst/.cloudburrow-seeded; sync"
	out, err := b.podExec(ctx, name, nil, script)
	if err != nil {
		return "", fmt.Errorf("import disk.raw's root file system: %w", err)
	}
	return string(out), nil
}

// imageObjectReader opens an object of this instance's Cloud Storage for an image
// import; addr is the storage port's host address, "" when it is not served.
func imageObjectReader(addr func() string) func(ctx context.Context, bucket, object string) (io.ReadCloser, int64, error) {
	return func(ctx context.Context, bucket, object string) (io.ReadCloser, int64, error) {
		a := addr()
		if a == "" {
			return nil, 0, errors.New("Cloud Storage is not served on this instance")
		}
		c, err := storageProvider{endpoint: a}.storageClient(ctx)
		if err != nil {
			return nil, 0, err
		}
		rd, err := c.Bucket(bucket).Object(object).NewReader(ctx)
		if err != nil {
			_ = c.Close()
			if errors.Is(err, storage.ErrObjectNotExist) || errors.Is(err, storage.ErrBucketNotExist) {
				return nil, 0, fmt.Errorf("%s/%s: %w", bucket, object, compute.ErrObjectNotFound)
			}
			return nil, 0, err
		}
		return closeBoth{rd, c}, rd.Attrs.Size, nil
	}
}
