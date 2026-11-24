// Package hostmount gives a container a path of this machine when the
// container may run on another one (#2209).
//
// With local compute a container binds the path itself (`-v path:/dst`). With
// remote compute (internal/remotecompute) the docker CLI here drives the
// Docker of a remote host, where a bind mount would name the host's own
// filesystem, so the path's content is carried into a Docker volume on the
// host instead, labelled for the instance (dockergc removes it with the
// instance's other volumes), and the container mounts that volume:
//
//   - the content goes over the same Docker socket every other docker call
//     uses (the SSH connection's forward), as a tar stream into a helper
//     container that is created, never started, with the volume mounted
//     (`docker cp - helper:/d`); nothing else is needed on the host;
//   - each volume keeps a manifest here (path, size, time, mode) of what it
//     holds, so the next use sends only what changed and removes only what
//     was removed; a volume that is not the one the manifest names (a new
//     host, a moved instance) is filled again in full;
//   - a ReadWrite mount's writes are copied back here by Collect (`docker cp
//     helper:/d -`), mirroring the volume into the path; a Cache mount's
//     writes stay on the host (an npm cache, the XNNPACK cache).
//
// Nothing about the container changes but its mount: its network, user and
// egress stay as the caller made them.
package hostmount

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
)

// Mode is how a container uses the path.
type Mode int

// The modes.
const (
	// ReadOnly mounts read-only.
	ReadOnly Mode = iota
	// ReadWrite mounts writable; Collect copies the container's writes
	// back to the path.
	ReadWrite
	// Cache mounts writable; the container's writes stay on the compute
	// host for the next use, and the path here is not changed.
	Cache
)

// MountIDLabel names the volume's fill: the manifest here is for the
// volume with the same ID, and for no other volume of the same name.
const MountIDLabel = "cloudburrow.dev/mount-id"

// Config is the remote compute host's side, set by SetRemote.
type Config struct {
	// Instance labels the volumes (dockergc.Label) and names them.
	Instance string
	// StateDir holds the volumes' manifests, under StateDir/mounts.
	StateDir string
	// Helper is an image on the host whose /bin/true and /bin/rm the
	// helper containers use: the kind node image, which every instance's
	// host holds.
	Helper string
	// Docker is the docker CLI; empty is "docker".
	Docker string
}

var (
	mu     sync.Mutex
	remote *Config
	locks  = map[string]*sync.Mutex{}
)

// SetRemote makes every Bind from now on carry its path to the compute
// host; nil returns to plain bind mounts.
func SetRemote(c *Config) {
	mu.Lock()
	defer mu.Unlock()
	if c != nil {
		cp := *c
		if cp.Docker == "" {
			cp.Docker = "docker"
		}
		c = &cp
	}
	remote = c
}

// Remote reports whether the compute is a remote host.
func Remote() bool {
	mu.Lock()
	defer mu.Unlock()
	return remote != nil
}

func current() *Config {
	mu.Lock()
	defer mu.Unlock()
	return remote
}

func lockFor(name string) *sync.Mutex {
	mu.Lock()
	defer mu.Unlock()
	l := locks[name]
	if l == nil {
		l = &sync.Mutex{}
		locks[name] = l
	}
	return l
}

// Mount is one path as a container mounts it.
type Mount struct {
	local, dst string
	mode       Mode
	cfg        *Config // nil: a bind mount of local
	volume     string
	file       string // the file's name in the volume, for a file
	scratch    bool
}

// Bind prepares local (a file or a directory) for a container that mounts
// it at dst. With remote compute the volume persists between uses, so an
// unchanged path is not sent again.
func Bind(ctx context.Context, local, dst string, mode Mode) (*Mount, error) {
	return prepare(ctx, local, dst, mode, false)
}

// Scratch is Bind for a path used once (a job's temporary directory):
// Release removes its volume.
func Scratch(ctx context.Context, local, dst string, mode Mode) (*Mount, error) {
	return prepare(ctx, local, dst, mode, true)
}

func prepare(ctx context.Context, local, dst string, mode Mode, scratch bool) (*Mount, error) {
	abs, err := filepath.Abs(local)
	if err != nil {
		return nil, err
	}
	m := &Mount{local: abs, dst: dst, mode: mode, cfg: current(), scratch: scratch}
	if m.cfg == nil {
		return m, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("mount %s on the compute host: %w", local, err)
	}
	if !info.IsDir() {
		m.file = info.Name()
	}
	m.volume = volumeName(m.cfg.Instance, abs, mode != ReadOnly, scratch)
	l := lockFor(m.volume)
	l.Lock()
	defer l.Unlock()
	if err := m.push(ctx); err != nil {
		return nil, fmt.Errorf("copy %s to the compute host: %w", local, err)
	}
	return m, nil
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// volumeName is the path's volume: one per instance, path and writability,
// so a read-only and a writable use of one path do not share modes.
func volumeName(instance, abs string, writable, scratch bool) string {
	kind := "ro"
	if writable {
		kind = "rw"
	}
	sum := sha256.Sum256([]byte(instance + "\x00" + abs + "\x00" + kind))
	name := "cb-mount-" + unsafeName.ReplaceAllString(instance, "-") + "-" + hex.EncodeToString(sum[:8])
	if scratch {
		name += "-" + randomHex(4)
	}
	return name
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Args are the docker run arguments that mount it.
func (m *Mount) Args() []string {
	if m.cfg == nil {
		v := m.local + ":" + m.dst
		if m.mode == ReadOnly {
			v += ":ro"
		}
		return []string{"-v", v}
	}
	spec := "type=volume,src=" + m.volume + ",dst=" + m.dst
	if m.file != "" {
		// A file is the one entry of its volume, mounted by itself.
		spec += ",volume-subpath=" + m.file
	}
	if m.mode == ReadOnly {
		spec += ",readonly"
	}
	return []string{"--mount", spec}
}

// Volume is the volume on the compute host, or "" for a bind mount.
func (m *Mount) Volume() string { return m.volume }

// Collect copies a ReadWrite mount's content on the compute host back to
// the path, after the container has written it. It does nothing for a bind
// mount or another mode.
func (m *Mount) Collect(ctx context.Context) error {
	if m == nil || m.cfg == nil || m.mode != ReadWrite {
		return nil
	}
	l := lockFor(m.volume)
	l.Lock()
	defer l.Unlock()
	if err := m.pull(ctx); err != nil {
		return fmt.Errorf("copy %s back from the compute host: %w", m.local, err)
	}
	return nil
}

// Release removes a Scratch mount's volume and manifest. It does nothing
// for a bind mount or a Bind.
func (m *Mount) Release() {
	if m == nil || m.cfg == nil || !m.scratch {
		return
	}
	m.Drop()
}

// Drop removes the mount's volume and manifest on the compute host, Bind's
// too, once the path is no longer used (a request's file). It does nothing
// for a bind mount.
func (m *Mount) Drop() {
	if m == nil || m.cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = m.docker(ctx, nil, nil, "volume", "rm", "-f", m.volume)
	_ = os.Remove(m.manifestPath())
}

// ---- manifests ----

// entry is one file, directory or link as the volume holds it.
type entry struct {
	Dir   bool        `json:"dir,omitempty"`
	Link  string      `json:"link,omitempty"`
	Size  int64       `json:"size,omitempty"`
	MTime int64       `json:"mtime,omitempty"` // UnixNano
	Mode  fs.FileMode `json:"mode"`
}

type manifest struct {
	MountID string           `json:"mountId"`
	Entries map[string]entry `json:"entries"`
}

func (m *Mount) manifestPath() string {
	return filepath.Join(m.cfg.StateDir, "mounts", m.volume+".json")
}

func (m *Mount) loadManifest() manifest {
	var mf manifest
	b, err := os.ReadFile(m.manifestPath())
	if err == nil {
		_ = json.Unmarshal(b, &mf)
	}
	if mf.Entries == nil {
		mf.Entries = map[string]entry{}
	}
	return mf
}

func (m *Mount) saveManifest(mf manifest) error {
	if err := os.MkdirAll(filepath.Dir(m.manifestPath()), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(mf)
	if err != nil {
		return err
	}
	tmp := m.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.manifestPath())
}

// walk lists the path: its tree for a directory, its one entry for a file.
func (m *Mount) walk() (map[string]entry, error) {
	out := map[string]entry{}
	if m.file != "" {
		info, err := os.Stat(m.local)
		if err != nil {
			return nil, err
		}
		out[m.file] = entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), Mode: info.Mode().Perm()}
		return out, nil
	}
	err := filepath.WalkDir(m.local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == m.local {
			return nil
		}
		rel, err := filepath.Rel(m.local, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = entry{Link: target, Mode: 0o777}
		case d.IsDir():
			out[rel] = entry{Dir: true, Mode: info.Mode().Perm()}
		case info.Mode().IsRegular():
			out[rel] = entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), Mode: info.Mode().Perm()}
		}
		return nil
	})
	return out, err
}

// ---- the host side ----

func (m *Mount) docker(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.cfg.Docker, args...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// ensureVolume makes sure the volume exists and says whether the manifest
// describes it.
func (m *Mount) ensureVolume(ctx context.Context, mf *manifest) error {
	id, err := m.docker(ctx, nil, nil, "volume", "inspect", "--format", `{{index .Labels "`+MountIDLabel+`"}}`, m.volume)
	if err == nil && id != "" && id == mf.MountID {
		return nil
	}
	if err == nil {
		// A volume this manifest is not about: start it afresh, unless a
		// container holds it (then fill it in full over what it has).
		if _, rmErr := m.docker(ctx, nil, nil, "volume", "rm", m.volume); rmErr != nil {
			mf.MountID, mf.Entries = id, map[string]entry{}
			return nil
		}
	}
	mf.MountID, mf.Entries = randomHex(12), map[string]entry{}
	args := append([]string{"volume", "create"}, dockergc.LabelArgs(m.cfg.Instance)...)
	args = append(args, "--label", MountIDLabel+"="+mf.MountID, m.volume)
	if _, err := m.docker(ctx, nil, nil, args...); err != nil {
		return err
	}
	if m.mode == ReadOnly || m.file != "" {
		return nil
	}
	// The volume's root is root's 0755, as Docker makes it: writable by
	// the container's user, whoever the image runs as.
	return m.inVolume(ctx, "/bin/chmod", "0777", "/d")
}

// inVolume runs one command of the helper image with the volume at /d.
func (m *Mount) inVolume(ctx context.Context, command string, args ...string) error {
	full := append([]string{"run", "--rm", "--network", "none"}, dockergc.LabelArgs(m.cfg.Instance)...)
	full = append(full, "--mount", "type=volume,src="+m.volume+",dst=/d", "--entrypoint", command, m.cfg.Helper)
	_, err := m.docker(ctx, nil, nil, append(full, args...)...)
	return err
}

// helper creates a container (never started) with the volume at /d.
func (m *Mount) helper(ctx context.Context) (string, func(), error) {
	args := append([]string{"create", "--network", "none"}, dockergc.LabelArgs(m.cfg.Instance)...)
	args = append(args, "--mount", "type=volume,src="+m.volume+",dst=/d", "--entrypoint", "/bin/true", m.cfg.Helper)
	id, err := m.docker(ctx, nil, nil, args...)
	if err != nil {
		return "", nil, err
	}
	return id, func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = m.docker(c, nil, nil, "rm", "-f", "-v", id)
	}, nil
}

// remove deletes rels from the volume, deepest first.
func (m *Mount) remove(ctx context.Context, rels []string) error {
	sort.Slice(rels, func(i, j int) bool { return len(rels[i]) > len(rels[j]) })
	for len(rels) > 0 {
		n := min(len(rels), 200)
		args := []string{"-rf", "--"}
		for _, r := range rels[:n] {
			args = append(args, "/d/"+r)
		}
		if err := m.inVolume(ctx, "/bin/rm", args...); err != nil {
			return err
		}
		rels = rels[n:]
	}
	return nil
}

// push makes the volume hold the path's content now.
func (m *Mount) push(ctx context.Context) error {
	mf := m.loadManifest()
	if err := m.ensureVolume(ctx, &mf); err != nil {
		return err
	}
	cur, err := m.walk()
	if err != nil {
		return err
	}
	var gone, changed []string
	for rel := range mf.Entries {
		if _, ok := cur[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	for rel, e := range cur {
		if old, ok := mf.Entries[rel]; !ok || old != e {
			changed = append(changed, rel)
		}
	}
	if len(gone) > 0 {
		if err := m.remove(ctx, gone); err != nil {
			return err
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed) // a directory before what is in it
		id, done, err := m.helper(ctx)
		if err != nil {
			return err
		}
		defer done()
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(m.writeTar(pw, changed, cur)) }()
		if _, err := m.docker(ctx, pr, nil, "cp", "-", id+":/d"); err != nil {
			_ = pr.CloseWithError(err)
			return err
		}
	}
	mf.Entries = cur
	return m.saveManifest(mf)
}

// hostMode is the mode an entry has in the volume: readable by any user
// for a read-only mount, and writable by any for a writable one, as the
// files are root's there and the container's user is the image's.
func (m *Mount) hostMode(e entry) int64 {
	mode := e.Mode.Perm()
	switch {
	case m.mode != ReadOnly && e.Dir:
		mode = 0o777
	case m.mode != ReadOnly:
		mode |= 0o666
	case e.Dir:
		mode |= 0o555
	default:
		mode |= 0o444
	}
	return int64(mode)
}

func (m *Mount) writeTar(w io.Writer, rels []string, cur map[string]entry) error {
	tw := tar.NewWriter(w)
	for _, rel := range rels {
		e := cur[rel]
		h := &tar.Header{Name: rel, Mode: m.hostMode(e), ModTime: time.Unix(0, e.MTime), Format: tar.FormatPAX}
		src := filepath.Join(m.local, filepath.FromSlash(rel))
		if m.file != "" {
			src = m.local
		}
		switch {
		case e.Dir:
			h.Typeflag, h.Name = tar.TypeDir, rel+"/"
			h.ModTime = time.Now()
		case e.Link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, e.Size
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := os.Open(src)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, e.Size)
			f.Close()
			if err != nil {
				return fmt.Errorf("%s changed while it was being copied: %w", src, err)
			}
		}
	}
	return tw.Close()
}

// pull mirrors the volume into the path: what the container wrote comes
// here, and what it removed goes.
func (m *Mount) pull(ctx context.Context) error {
	id, done, err := m.helper(ctx)
	if err != nil {
		return err
	}
	defer done()
	pr, pw := io.Pipe()
	seen := map[string]bool{}
	extracted := make(chan error, 1)
	go func() {
		err := m.extract(pr, seen)
		_ = pr.CloseWithError(err)
		extracted <- err
	}()
	_, derr := m.docker(ctx, nil, pw, "cp", id+":/d/.", "-")
	_ = pw.CloseWithError(derr)
	xerr := <-extracted
	if derr != nil {
		return derr
	}
	if xerr != nil {
		return xerr
	}
	if m.file == "" {
		cur, err := m.walk()
		if err != nil {
			return err
		}
		var gone []string
		for rel := range cur {
			if !seen[rel] {
				gone = append(gone, rel)
			}
		}
		sort.Slice(gone, func(i, j int) bool { return len(gone[i]) > len(gone[j]) })
		for _, rel := range gone {
			_ = os.RemoveAll(filepath.Join(m.local, filepath.FromSlash(rel)))
		}
	}
	cur, err := m.walk()
	if err != nil {
		return err
	}
	mf := m.loadManifest()
	mf.Entries = cur
	return m.saveManifest(mf)
}

// within is the entry's place under root, or false for a name that would
// leave it.
func within(root, name string) (string, string, bool) {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return "", "", false
	}
	rel := strings.TrimPrefix(clean, "/")
	return rel, filepath.Join(root, filepath.FromSlash(rel)), true
}

// extract writes the volume's tar here. docker cp names the entries under
// the copied directory's own name ("./" or "d/"), which is dropped.
func (m *Mount) extract(r io.Reader, seen map[string]bool) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if i := strings.IndexByte(name, '/'); i >= 0 && (name[:i] == "d" || name[:i] == ".") {
			name = name[i+1:]
		} else if name == "d" || name == "." {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			continue
		}
		rel, dst, ok := within(m.local, name)
		if !ok {
			continue
		}
		if m.file != "" {
			if rel != m.file {
				continue
			}
			dst = m.local
		}
		seen[rel] = true
		perm := os.FileMode(h.Mode).Perm() &^ 0o022
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// A link out of the path is not followed back here.
			if filepath.IsAbs(h.Linkname) || strings.HasPrefix(path.Clean(path.Join(path.Dir(rel), h.Linkname)), "../") {
				continue
			}
			_ = os.RemoveAll(dst)
			if err := os.Symlink(h.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if info, err := os.Lstat(dst); err == nil {
				if info.Mode().IsRegular() {
					perm = info.Mode().Perm()
				} else {
					_ = os.RemoveAll(dst)
				}
			}
			tmp := dst + ".cb-collect"
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(tmp)
				return err
			}
			_ = os.Chmod(tmp, perm)
			_ = os.Chtimes(tmp, h.ModTime, h.ModTime)
			if err := os.Rename(tmp, dst); err != nil {
				return err
			}
		}
	}
}
