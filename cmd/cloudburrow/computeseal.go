package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// A disk with a customer-managed or customer-supplied key is encrypted at
// all times (#2206, compute's diskseal.go): its claim holds only
// compute.CryptFile, a dm-crypt image (aes-xts-plain64) on the kind node.
// The node's own kernel serves the decrypted device: UnsealDisk attaches
// the image to a loop device, maps it with dm-crypt (dmsetup, the node
// image's) and mounts its file system under cryptRoot on the node; the
// VM's pod mounts that directory (hostPath), as it would the claim. Every
// block the guest writes reaches the claim encrypted, so the bytes on this
// machine's disk are ciphertext while the VM runs as well as after.
// SealDisk unmounts and removes the mapping once no running VM has the
// disk. The dm-crypt key is derived here from the disk's data key and
// written to dmsetup's standard input through docker exec: it is held by
// the kernel alone, never in a file, an argument or the cluster's API.

// cryptRoot is where the node mounts the open encrypted disks.
const cryptRoot = "/var/lib/cloudburrow-crypt"

var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// cryptName is the device-mapper name of a claim: unique across the
// instances sharing this machine's Docker VM (the kernel's names are
// global), at most 127 bytes.
func (b *computeVMs) cryptName(claim string) string {
	h := sha256.Sum256([]byte(b.cfg.Name + "/" + claim))
	n := "cloudburrow-" + regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(strings.ToLower(b.cfg.Name), "-")
	if len(n) > 60 {
		n = n[:60]
	}
	return n + "-" + hex.EncodeToString(h[:12])
}

func (b *computeVMs) cryptMount(claim string) string { return cryptRoot + "/" + b.cryptName(claim) }

// claimDir is a claim's node (its kind container) and directory there,
// binding the claim first when no pod has used it yet (kind's local-path
// class binds a claim at its first consumer).
func (b *computeVMs) claimDir(ctx context.Context, claim string) (node, dir string, err error) {
	for try := 0; ; try++ {
		pv, err := b.kubectl(ctx, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
		if err != nil {
			return "", "", err
		}
		if pv = strings.TrimSpace(pv); pv != "" {
			path, err := b.kubectl(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.local.path}{.spec.hostPath.path}")
			if err != nil {
				return "", "", err
			}
			node, _ := b.kubectl(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0].values[0]}")
			node = strings.TrimSpace(node)
			if node == "" {
				node = b.cfg.ClusterName() + "-control-plane"
			}
			path = strings.TrimSpace(path)
			if !safePath.MatchString(path) || strings.Contains(path, "..") || !safePath.MatchString("/"+node) {
				return "", "", fmt.Errorf("the volume of %s is at an unexpected path %q on %q", claim, path, node)
			}
			return node, path, nil
		}
		if try > 0 {
			return "", "", fmt.Errorf("the claim %s is not bound", claim)
		}
		_, done, err := b.helperPod(ctx, claim)
		if err != nil {
			return "", "", err
		}
		done()
	}
}

// nodeSh runs script with sh on the kind node, args as $1..., stdin as
// its standard input.
func nodeSh(ctx context.Context, node string, stdin io.Reader, script string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", "-e", "DM_DISABLE_UDEV=1", node, "sh", "-c", script, "sh"}, args...)...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(vmTail(errb.String(), 4)))
	}
	return out.String(), nil
}

// The node scripts. Each reads the hex dm-crypt key (and a header) from
// standard input with the shell's read: printf is a built-in, so the key
// is in no process's arguments.
const (
	// cryptOpenSh maps $2 (an image) as $1 and mounts it at $3.
	cryptOpenSh = `set -e
name=$1; img=$2; mnt=$3
IFS= read -r key
if ! dmsetup info "$name" >/dev/null 2>&1; then
  size=$(stat -c %s "$img")
  sectors=$(( (size - 4096) / 512 ))
  loop=$(losetup -f --show "$img")
  if ! printf '0 %s crypt aes-xts-plain64 %s 0 %s 8\n' "$sectors" "$key" "$loop" | dmsetup create "$name" --noudevsync; then
    losetup -d "$loop"; exit 1
  fi
  # Detached while the mapping holds it: the kernel frees it with the mapping.
  losetup -d "$loop"
fi
dmsetup mknodes "$name" >/dev/null 2>&1 || true
mkdir -p "$mnt"
mountpoint -q "$mnt" || mount /dev/mapper/"$name" "$mnt"
`
	// cryptFormatSh makes $2 a new image of $4 bytes, its header the second
	// line of standard input, with an empty ext4 file system, mapped as $1
	// and mounted at $3.
	cryptFormatSh = `set -e
name=$1; img=$2; mnt=$3; size=$4
IFS= read -r key; IFS= read -r header
rm -f "$img"
printf '%s\n' "$header" > "$img"
truncate -s $((4096 + size)) "$img"
loop=$(losetup -f --show "$img")
if ! printf '0 %s crypt aes-xts-plain64 %s 0 %s 8\n' "$((size / 512))" "$key" "$loop" | dmsetup create "$name" --noudevsync; then
  losetup -d "$loop"; exit 1
fi
losetup -d "$loop"
dmsetup mknodes "$name" >/dev/null 2>&1 || true
mkfs.ext4 -q -F -m 0 -E lazy_itable_init=1,lazy_journal_init=1,nodiscard /dev/mapper/"$name"
mkdir -p "$mnt"
mount /dev/mapper/"$name" "$mnt"
rm -rf "$mnt/lost+found"
`
	// cryptCloseSh unmounts $2 and removes the mapping $1.
	cryptCloseSh = `name=$1; mnt=$2
i=0
while mountpoint -q "$mnt"; do
  umount "$mnt" 2>/dev/null && break
  i=$((i+1)); [ $i -lt 60 ] || { echo "$mnt is busy" >&2; exit 1; }; sleep 1
done
i=0
while dmsetup info "$name" >/dev/null 2>&1; do
  dmsetup remove --noudevsync "$name" 2>/dev/null && break
  i=$((i+1)); [ $i -lt 60 ] || { echo "$name is busy" >&2; exit 1; }; sleep 1
done
rmdir "$mnt" 2>/dev/null
exit 0
`
	// cryptGrowSh grows the image $2 to $3 bytes, and the open mapping $1
	// and its file system with it.
	cryptGrowSh = `set -e
name=$1; img=$2; size=$3
[ $(stat -c %s "$img") -lt $((4096 + size)) ] || exit 0
truncate -s $((4096 + size)) "$img"
if dmsetup info "$name" >/dev/null 2>&1; then
  set -- $(dmsetup table --showkeys "$name")
  loop=$(losetup -j "$img" -O NAME -n | head -n 1)
  [ -n "$loop" ] && losetup -c "$loop"
  printf '0 %s %s %s %s %s %s %s\n' "$((size / 512))" "$3" "$4" "$5" "$6" "$7" "$8" | dmsetup reload "$name" --noudevsync
  dmsetup resume "$name" --noudevsync
  resize2fs /dev/mapper/"$name" >/dev/null 2>&1
fi
`
)

// header is the first line of a claim's image, "" when it has none.
func (b *computeVMs) cryptHeaderAt(ctx context.Context, node, dir string) (string, bool, error) {
	out, err := nodeSh(ctx, node, nil, `if [ -e "$1" ]; then head -c 200 "$1" | head -n 1; echo; echo yes; else echo no; fi`, dir+"/"+compute.CryptFile)
	if err != nil {
		return "", false, err
	}
	l := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(l) == 0 || l[len(l)-1] != "yes" {
		return "", false, nil
	}
	return l[0], true, nil
}

func keyInput(keys ...string) io.Reader { return strings.NewReader(strings.Join(keys, "\n") + "\n") }

// openCrypt maps and mounts the image in dir with dataKey at mnt (as
// name), after checking the key against its header.
func (b *computeVMs) openCrypt(ctx context.Context, node, dir, name, mnt string, dataKey []byte) error {
	h, ok, err := b.cryptHeaderAt(ctx, node, dir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the encrypted disk has no %s", compute.CryptFile)
	}
	if err := compute.CheckCryptHeader(h, dataKey); err != nil {
		return err
	}
	k, err := compute.CryptKey(dataKey)
	if err != nil {
		return err
	}
	if _, err := nodeSh(ctx, node, keyInput(k), cryptOpenSh, name, dir+"/"+compute.CryptFile, mnt); err != nil {
		return fmt.Errorf("open the encrypted disk: %w", err)
	}
	return nil
}

func closeCrypt(ctx context.Context, node, name, mnt string) error {
	if _, err := nodeSh(ctx, node, nil, cryptCloseSh, name, mnt); err != nil {
		return fmt.Errorf("close the encrypted disk: %w", err)
	}
	return nil
}

// rekey leaves a claim whose files came from a source encrypted with from
// (or in the clear) encrypted with to (or in the clear), its image sizeGb.
func (b *computeVMs) rekey(ctx context.Context, claim string, sizeGb int64, from, to []byte) error {
	if from == nil && to == nil {
		return nil
	}
	node, dir, err := b.claimDir(ctx, claim)
	if err != nil {
		return err
	}
	_, has, err := b.cryptHeaderAt(ctx, node, dir)
	if err != nil {
		return err
	}
	if from != nil && to != nil && bytes.Equal(from, to) && has {
		return nil
	}
	if !has {
		from = nil // the files are in the clear (a blank disk, or a plain source)
	}
	base := b.cryptName(claim)
	src := dir
	if from != nil {
		src = cryptRoot + "/" + base + "-src"
		if err := b.openCrypt(ctx, node, dir, base+"-src", src, from); err != nil {
			return err
		}
		defer func() { _ = closeCrypt(context.Background(), node, base+"-src", src) }()
	}
	dst := dir
	if to != nil {
		if sizeGb <= 0 {
			sizeGb = 10
		}
		k, err := compute.CryptKey(to)
		if err != nil {
			return err
		}
		h, _ := compute.CryptHeader(to)
		dst = cryptRoot + "/" + base + "-new"
		if _, err := nodeSh(ctx, node, keyInput(k, h), cryptFormatSh, base+"-new", dir+"/"+compute.CryptFile+".new", dst,
			strconv.FormatInt(sizeGb<<30, 10)); err != nil {
			_ = closeCrypt(context.Background(), node, base+"-new", dst)
			return fmt.Errorf("format the encrypted disk: %w", err)
		}
		defer func() { _ = closeCrypt(context.Background(), node, base+"-new", dst) }()
	}
	// Copy the files across, then leave only the result in the claim.
	if _, err := nodeSh(ctx, node, nil, `set -e
cd "$1" && tar --numeric-owner --exclude=./`+compute.CryptFile+`* -cpf - . | tar --numeric-owner -xpf - -C "$2"
sync`, src, dst); err != nil {
		return fmt.Errorf("re-encrypt the disk: %w", err)
	}
	if to != nil {
		if err := closeCrypt(ctx, node, base+"-new", dst); err != nil {
			return err
		}
		if from != nil {
			if err := closeCrypt(ctx, node, base+"-src", src); err != nil {
				return err
			}
		}
		_, err = nodeSh(ctx, node, nil, `set -e
cd "$1" && find . -mindepth 1 -maxdepth 1 ! -name `+compute.CryptFile+`.new -exec rm -rf {} +
mv `+compute.CryptFile+`.new `+compute.CryptFile+` && sync`, dir)
		return err
	}
	if err := closeCrypt(ctx, node, base+"-src", src); err != nil {
		return err
	}
	_, err = nodeSh(ctx, node, nil, `rm -f "$1" && sync`, dir+"/"+compute.CryptFile)
	return err
}

// SealDisk closes an open encrypted disk: unmounted from the node and
// unmapped, its claim ciphertext as ever.
func (b *computeVMs) SealDisk(ctx context.Context, d compute.DiskSpec) error {
	if d.DataKey == nil || d.Open {
		return nil
	}
	node := b.cryptNode(d.Key)
	if node == "" {
		n, _, err := b.claimDir(ctx, d.Key)
		if err != nil {
			return err
		}
		node = n
	}
	return closeCrypt(ctx, node, b.cryptName(d.Key), b.cryptMount(d.Key))
}

// UnsealDisk opens an encrypted disk for a VM: its device mapped and
// mounted on the node at cryptMount, where the VM's pod mounts it.
func (b *computeVMs) UnsealDisk(ctx context.Context, d compute.DiskSpec) error {
	if d.DataKey == nil {
		return nil
	}
	if err := b.installer.Apply(ctx, pvcManifest(b.ns(), d.Key, d.SizeGb, "disk")); err != nil {
		return err
	}
	node, dir, err := b.claimDir(ctx, d.Key)
	if err != nil {
		return err
	}
	if _, has, err := b.cryptHeaderAt(ctx, node, dir); err != nil {
		return err
	} else if !has {
		// Files in the clear (a disk seeded before it had a key):
		// encrypted in place first.
		if err := b.rekey(ctx, d.Key, d.SizeGb, nil, d.DataKey); err != nil {
			return err
		}
	}
	if err := b.openCrypt(ctx, node, dir, b.cryptName(d.Key), b.cryptMount(d.Key), d.DataKey); err != nil {
		return err
	}
	b.mu.Lock()
	if b.cryptNodes == nil {
		b.cryptNodes = map[string]string{}
	}
	b.cryptNodes[d.Key] = node
	b.mu.Unlock()
	return nil
}

func (b *computeVMs) cryptNode(claim string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cryptNodes[claim]
}

// growCrypt grows an encrypted disk's image (and its open device and file
// system) to its new size.
func (b *computeVMs) growCrypt(ctx context.Context, d compute.DiskSpec) error {
	node, dir, err := b.claimDir(ctx, d.Key)
	if err != nil {
		return err
	}
	_, err = nodeSh(ctx, node, nil, cryptGrowSh, b.cryptName(d.Key), dir+"/"+compute.CryptFile, strconv.FormatInt(d.SizeGb<<30, 10))
	return err
}

// cryptVolume is the pod volume of an open encrypted disk: the node's
// mount of its decrypted device.
func (b *computeVMs) cryptVolume(name string, d compute.DiskSpec) map[string]any {
	return map[string]any{"name": name, "hostPath": map[string]any{"path": b.cryptMount(d.Key), "type": "Directory"}}
}

// helperPod starts a pod with the claim mounted at /d (which binds it),
// waits for it and returns its name and a function that deletes it.
func (b *computeVMs) helperPod(ctx context.Context, claim string) (string, func(), error) {
	ref, err := b.ensureImage(ctx, compute.PublicImages[0])
	if err != nil {
		return "", nil, err
	}
	name := "bind-" + claim
	if len(name) > 63 {
		name = name[:63]
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": b.ns(), "labels": map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute"}},
		"spec": map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "terminationGracePeriodSeconds": 0,
			"containers": []any{map[string]any{"name": "bind", "image": ref, "imagePullPolicy": "Never",
				"command":      []string{"/bin/sh", "-c", "sleep 3600"},
				"volumeMounts": []any{map[string]any{"name": "d", "mountPath": "/d"}}}},
			"volumes": []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim}}}}}
	m, _ := json.Marshal(pod)
	_, _ = b.kubectl(ctx, "delete", "pod", name, "--ignore-not-found", "--wait=true")
	if err := b.installer.Apply(ctx, string(m)); err != nil {
		return "", nil, err
	}
	done := func() {
		_, _ = b.kubectl(context.Background(), "delete", "pod", name, "--ignore-not-found", "--wait=true", "--timeout=2m")
	}
	if _, err := b.kubectl(ctx, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=5m"); err != nil {
		done()
		return "", nil, fmt.Errorf("bind %s: %w", claim, err)
	}
	return name, done, nil
}
