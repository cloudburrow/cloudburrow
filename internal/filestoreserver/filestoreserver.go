// Package filestoreserver runs Filestore instances' NFS servers (#2192):
// for each instance, NFS-Ganesha (Debian's package, LGPL-3.0, in an image
// built here from image/, never linked or vendored) in a privileged
// container on the Docker network the instance's kind clusters share, at a
// fixed address of that network, so a Compute Engine VM's pod or a
// Kubernetes Engine cluster's pod mounts ADDRESS:/SHARE over NFSv3 (and
// NFSv4.1 where the tier offers it), as on Google.
//
// An instance's files are a Docker volume, /export/data/SHARE, with its
// snapshots beside them in /export/snapshots/SHARE/KEY; a backup is a tar
// file of the share in Dir, kept after its instance is gone. The volumes
// and containers carry the label io.cloudburrow.filestore=INSTANCE, not
// the instance label `stop` cleans up by, so their data survives
// `cloudburrow stop` and `up` in persistent mode; Stop removes them in
// ephemeral mode, and `cloudburrow delete` with RemoveAll.
package filestoreserver

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

//go:embed image/Dockerfile
var dockerfile []byte

//go:embed image/entrypoint.sh
var entrypoint []byte

// Label is the label on an instance's NFS containers and volumes.
const Label = "io.cloudburrow.filestore"

// Network is the Docker network kind's clusters share.
const Network = "kind"

// Runner runs a program; injected for tests.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error
}

// ExecRunner runs programs with os/exec.
type ExecRunner struct{}

// Run runs name with args.
func (ExecRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var errb strings.Builder
	cmd.Stderr = &errb
	var outb strings.Builder
	if stdout == nil {
		cmd.Stdout = &outb
	} else {
		cmd.Stdout = stdout
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(outb.String())
		}
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(2, len(args))], " "), err, msg)
	}
	return nil
}

// Docker is the backend.
type Docker struct {
	// Instance is the CloudBurrow instance's name.
	Instance string
	// Dir holds the backups.
	Dir string
	// Runner runs docker; nil is ExecRunner.
	Runner Runner
	// Logf reports progress; nil is silent.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	built string
	// locks serialise the work on one instance's server.
	locks map[string]*sync.Mutex
}

var _ filestore.Backend = (*Docker)(nil)

func (d *Docker) runner() Runner {
	if d.Runner == nil {
		return ExecRunner{}
	}
	return d.Runner
}

func (d *Docker) docker(ctx context.Context, args ...string) (string, error) {
	var out strings.Builder
	err := d.runner().Run(ctx, nil, &out, "docker", args...)
	return strings.TrimSpace(out.String()), err
}

func (d *Docker) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d *Docker) lock(key string) func() {
	d.mu.Lock()
	if d.locks == nil {
		d.locks = map[string]*sync.Mutex{}
	}
	l, ok := d.locks[key]
	if !ok {
		l = &sync.Mutex{}
		d.locks[key] = l
	}
	d.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Tag is the image's tag: the hash of its recipe.
func Tag() string {
	h := sha256.New()
	h.Write(dockerfile)
	h.Write(entrypoint)
	return "cloudburrow.local/filestore-nfs:" + hex.EncodeToString(h.Sum(nil))[:12]
}

// image builds the server image once.
func (d *Docker) image(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tag := Tag()
	if d.built == tag {
		return tag, nil
	}
	if _, err := d.docker(ctx, "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		d.built = tag
		return tag, nil
	}
	dir, err := os.MkdirTemp("", "cloudburrow-filestore-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), entrypoint, 0o755); err != nil {
		return "", err
	}
	d.logf("filestore: building the NFS server image %s", tag)
	args := append([]string{"build", "-q", "-t", tag}, dockergc.LabelArgs(d.Instance)...)
	if _, err := d.docker(ctx, append(args, dir)...); err != nil {
		return "", fmt.Errorf("build the NFS server image: %w", err)
	}
	d.built = tag
	return tag, nil
}

func (d *Docker) container(key string) string {
	return "cloudburrow-" + d.Instance + "-filestore-" + key
}

func (d *Docker) volume(key string) string { return d.container(key) }

func (d *Docker) label() string { return Label + "=" + d.Instance }

// Ensure runs key's server with spec's configuration, at ip if given:
// unchanged when its container already runs that configuration, or made
// again over the same volume.
func (d *Docker) Ensure(ctx context.Context, key, ip string, spec filestore.Spec) (string, error) {
	defer d.lock(key)()
	conf := Config(spec)
	sum := sha256.Sum256([]byte(conf))
	confHash := hex.EncodeToString(sum[:8])
	name := d.container(key)
	img, err := d.image(ctx)
	if err != nil {
		return "", err
	}
	if out, err := d.docker(ctx, "container", "inspect", "--format",
		`{{index .Config.Labels "io.cloudburrow.filestore.config"}} {{.State.Running}} {{.Config.Image}}`, name); err == nil {
		f := strings.Fields(out)
		if len(f) == 3 && f[0] == confHash && f[2] == img && ip != "" {
			if f[1] != "true" {
				if _, err := d.docker(ctx, "start", name); err != nil {
					return "", err
				}
				return ip, d.waitReady(ctx, name)
			}
			return ip, nil
		}
		if _, err := d.docker(ctx, "rm", "-f", name); err != nil {
			return "", err
		}
	}
	if _, err := d.docker(ctx, "volume", "inspect", d.volume(key)); err != nil {
		if _, err := d.docker(ctx, "volume", "create", "--label", d.label(), "--label", "cloudburrow.dev/owned=true", d.volume(key)); err != nil {
			return "", err
		}
	}
	if ip == "" {
		if ip, err = d.allocate(ctx, key); err != nil {
			return "", err
		}
	}
	_, err = d.docker(ctx, "run", "-d", "--name", name, "--hostname", "filestore-"+key,
		"--label", d.label(), "--label", "cloudburrow.dev/owned=true", "--label", "io.cloudburrow.filestore.config="+confHash,
		"--privileged", "--network", Network, "--ip", ip,
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"-e", "GANESHA_CONFIG="+base64.StdEncoding.EncodeToString([]byte(conf)),
		"-v", d.volume(key)+":/export", img)
	if err != nil {
		return "", fmt.Errorf("start the NFS server: %w", err)
	}
	return ip, d.waitReady(ctx, name)
}

// waitReady waits until the server has registered NFS with rpcbind.
func (d *Docker) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := d.docker(ctx, "exec", name, "rpcinfo", "-p")
		if err == nil && strings.Contains(out, " nfs") && strings.Contains(out, "mountd") {
			return nil
		}
		if time.Now().After(deadline) {
			logs, _ := d.docker(ctx, "logs", "--tail", "20", name)
			return fmt.Errorf("the NFS server %s did not become ready in 90s: %v: %s", name, err, logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// allocate picks a free address for key in the network's IPv4 subnet,
// from the top quarter, which Docker's own allocation (from the bottom)
// reaches last.
func (d *Docker) allocate(ctx context.Context, key string) (string, error) {
	out, err := d.docker(ctx, "network", "inspect", Network, "--format",
		`{{range .IPAM.Config}}{{.Subnet}} {{end}}|{{range .Containers}}{{.IPv4Address}} {{end}}`)
	if err != nil {
		return "", fmt.Errorf("read the %s network: %w", Network, err)
	}
	subnets, used, _ := strings.Cut(out, "|")
	inUse := map[string]bool{}
	for _, a := range strings.Fields(used) {
		ip, _, _ := strings.Cut(a, "/")
		inUse[ip] = true
	}
	// Stopped servers keep their addresses.
	if all, err := d.docker(ctx, "ps", "-a", "--filter", "label="+Label, "--format", "{{.Names}}"); err == nil {
		for _, n := range strings.Fields(all) {
			if a, err := d.docker(ctx, "container", "inspect", "--format", `{{with index .NetworkSettings.Networks "kind"}}{{with .IPAMConfig}}{{.IPv4Address}}{{end}}{{end}}`, n); err == nil && a != "" {
				inUse[a] = true
			}
		}
	}
	return pickIP(strings.Fields(subnets), key, inUse)
}

// pickIP is a free address of the first IPv4 subnet, in its top quarter,
// starting from a hash of key.
func pickIP(subnets []string, key string, inUse map[string]bool) (string, error) {
	for _, s := range subnets {
		_, n, err := net.ParseCIDR(s)
		if err != nil || n.IP.To4() == nil {
			continue
		}
		ones, bits := n.Mask.Size()
		size := uint32(1) << (bits - ones)
		if size < 16 {
			continue
		}
		base := uint32(n.IP.To4()[0])<<24 | uint32(n.IP.To4()[1])<<16 | uint32(n.IP.To4()[2])<<8 | uint32(n.IP.To4()[3])
		quarter := size / 4
		h := sha256.Sum256([]byte(key))
		start := (uint32(h[0])<<8 | uint32(h[1])) % (quarter - 2)
		for i := uint32(0); i < quarter-2; i++ {
			off := size - quarter + (start+i)%(quarter-2) + 1
			v := base + off
			ip := net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
			if !inUse[ip] {
				return ip, nil
			}
		}
		return "", fmt.Errorf("no free address on the %s network's %s", Network, s)
	}
	return "", fmt.Errorf("the %s network has no IPv4 subnet", Network)
}

// Remove deletes key's server, volume and snapshots.
func (d *Docker) Remove(ctx context.Context, key string) error {
	defer d.lock(key)()
	name := d.container(key)
	if _, err := d.docker(ctx, "rm", "-f", "-v", name); err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	if _, err := d.docker(ctx, "volume", "rm", d.volume(key)); err != nil && !strings.Contains(err.Error(), "no such volume") {
		return err
	}
	return nil
}

var shareRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var keyRE = regexp.MustCompile(`^[a-z]+-[0-9a-f]+$`)

func checkNames(share string, keys ...string) error {
	if !shareRE.MatchString(share) {
		return fmt.Errorf("bad share name %q", share)
	}
	for _, k := range keys {
		if !keyRE.MatchString(k) {
			return fmt.Errorf("bad key %q", k)
		}
	}
	return nil
}

func (d *Docker) exec(ctx context.Context, key, script string, args ...string) (string, error) {
	return d.docker(ctx, append([]string{"exec", d.container(key), "sh", "-c", script, "sh"}, args...)...)
}

// Usage is the bytes the share's files take.
func (d *Docker) Usage(ctx context.Context, key, share string) (int64, error) {
	if err := checkNames(share, key); err != nil {
		return 0, err
	}
	out, err := d.exec(ctx, key, `du -sb "/export/data/$1" | cut -f1`, share)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// Snapshot copies the share's files, hard links kept, as snap.
func (d *Docker) Snapshot(ctx context.Context, key, share, snap string) error {
	if err := checkNames(share, key, snap); err != nil {
		return err
	}
	defer d.lock(key)()
	_, err := d.exec(ctx, key, `set -e; mkdir -p "/export/snapshots/$1"; rm -rf "/export/snapshots/$1/$2.tmp"; cp -a "/export/data/$1/." "/export/snapshots/$1/$2.tmp/"; mv "/export/snapshots/$1/$2.tmp" "/export/snapshots/$1/$2"; sync`, share, snap)
	return err
}

// DeleteSnapshot deletes snap's copy.
func (d *Docker) DeleteSnapshot(ctx context.Context, key, share, snap string) error {
	if err := checkNames(share, key, snap); err != nil {
		return err
	}
	defer d.lock(key)()
	_, err := d.exec(ctx, key, `rm -rf "/export/snapshots/$1/$2"`, share, snap)
	return err
}

// Revert replaces the share's files with snap's, in place, so the export
// and its clients' mounts stay.
func (d *Docker) Revert(ctx context.Context, key, share, snap string) error {
	if err := checkNames(share, key, snap); err != nil {
		return err
	}
	defer d.lock(key)()
	_, err := d.exec(ctx, key, `set -e; test -d "/export/snapshots/$1/$2"; find "/export/data/$1" -mindepth 1 -delete; cp -a "/export/snapshots/$1/$2/." "/export/data/$1/"; sync`, share, snap)
	return err
}

func (d *Docker) backupFile(backup string) string { return filepath.Join(d.Dir, backup+".tar") }

// Backup writes a tar of the share's files to Dir and answers its size.
func (d *Docker) Backup(ctx context.Context, key, share, backup string) (int64, error) {
	if err := checkNames(share, key, backup); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return 0, err
	}
	defer d.lock(key)()
	tmp := d.backupFile(backup) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	err = d.runner().Run(ctx, nil, f, "docker", "exec", d.container(key), "tar", "-C", "/export/data/"+share, "--numeric-owner", "-cf", "-", ".")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("copy the share out: %w", err)
	}
	if err := os.Rename(tmp, d.backupFile(backup)); err != nil {
		return 0, err
	}
	st, err := os.Stat(d.backupFile(backup))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Restore replaces the share's files with backup's.
func (d *Docker) Restore(ctx context.Context, key, share, backup string) error {
	if err := checkNames(share, key, backup); err != nil {
		return err
	}
	f, err := os.Open(d.backupFile(backup))
	if err != nil {
		return fmt.Errorf("the backup's data is gone: %w", err)
	}
	defer f.Close()
	defer d.lock(key)()
	return d.runner().Run(ctx, f, nil, "docker", "exec", "-i", d.container(key), "sh", "-c",
		`set -e; find "/export/data/$1" -mindepth 1 -delete; tar -C "/export/data/$1" --numeric-owner -xpf -; sync`, "sh", share)
}

// DeleteBackup deletes backup's tar.
func (d *Docker) DeleteBackup(_ context.Context, backup string) error {
	if !keyRE.MatchString(backup) {
		return fmt.Errorf("bad backup key %q", backup)
	}
	if err := os.Remove(d.backupFile(backup)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Stop stops the instance's NFS servers, keeping their volumes (persistent
// mode), or removes servers, volumes and backups (remove).
func (d *Docker) Stop(ctx context.Context, remove bool) error {
	names, err := d.docker(ctx, "ps", "-a", "--filter", "label="+d.label(), "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	var errs []error
	list := strings.Fields(names)
	sort.Strings(list)
	for _, n := range list {
		if remove {
			_, err = d.docker(ctx, "rm", "-f", "-v", n)
		} else {
			_, err = d.docker(ctx, "stop", "-t", "5", n)
		}
		errs = append(errs, err)
	}
	if remove {
		errs = append(errs, RemoveVolumes(ctx, d.runner(), d.Instance))
		if d.Dir != "" {
			errs = append(errs, os.RemoveAll(d.Dir))
		}
	}
	return errors.Join(errs...)
}

// RemoveAll removes every NFS server container and volume of instance,
// for `cloudburrow delete`.
func RemoveAll(ctx context.Context, r Runner, instance string) error {
	d := &Docker{Instance: instance, Runner: r}
	return d.Stop(ctx, true)
}

// RemoveVolumes removes instance's NFS volumes.
func RemoveVolumes(ctx context.Context, r Runner, instance string) error {
	var out strings.Builder
	if err := r.Run(ctx, nil, &out, "docker", "volume", "ls", "-q", "--filter", "label="+Label+"="+instance); err != nil {
		return err
	}
	var errs []error
	for _, v := range strings.Fields(out.String()) {
		errs = append(errs, r.Run(ctx, nil, io.Discard, "docker", "volume", "rm", v))
	}
	return errors.Join(errs...)
}

// Config is NFS-Ganesha's configuration for spec: NFSv3 over TCP (UDP is
// not taken, as Filestore takes none) with mount paths by the pseudo path
// /SHARE, as Filestore's clients mount, and NFSv4.1 when spec asks; each
// share an export of /export/data/SHARE with its export options as client
// blocks, every client read-write without root squash when it has none.
func Config(spec filestore.Spec) string {
	var b strings.Builder
	protocols := "3"
	if spec.NFSv41 {
		protocols = "3, 4"
	}
	fmt.Fprintf(&b, `NFS_CORE_PARAM {
  Protocols = %s;
  NFS_Port = 2049;
  MNT_Port = 20048;
  NLM_Port = 32803;
  Enable_RQUOTA = false;
  Enable_UDP = false;
  Mount_Path_Pseudo = true;
}
NFSv4 {
  Minor_Versions = 1;
  Graceless = true;
  RecoveryBackend = fs;
}
`, protocols)
	for i, sh := range spec.Shares {
		fmt.Fprintf(&b, `EXPORT {
  Export_Id = %d;
  Path = /export/data/%s;
  Pseudo = /%s;
  Protocols = %s;
  Transports = TCP;
  SecType = sys;
  FSAL { Name = VFS; }
`, i+1, sh.Name, sh.Name, protocols)
		if len(sh.Exports) == 0 {
			b.WriteString("  Access_Type = RW;\n  Squash = No_Root_Squash;\n")
		} else {
			b.WriteString("  Access_Type = None;\n")
			for _, e := range sh.Exports {
				access, squash := "RW", "No_Root_Squash"
				if e.ReadOnly {
					access = "RO"
				}
				if e.RootSquash {
					squash = "Root_Squash"
				}
				fmt.Fprintf(&b, "  CLIENT {\n    Clients = %s;\n    Access_Type = %s;\n    Squash = %s;\n    Anonymous_Uid = %d;\n    Anonymous_Gid = %d;\n  }\n",
					strings.Join(e.IPRanges, ", "), access, squash, e.AnonUID, e.AnonGID)
			}
		}
		b.WriteString("}\n")
	}
	return b.String()
}
