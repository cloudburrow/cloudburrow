package remotecompute

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Carrying an instance's Docker state from one Docker to another (#2209):
// `cloudburrow cloud move` stops the instance, carries what its compute
// holds (the kind nodes, their volumes, every container and volume labelled
// for the instance, and the networks they join) to the other Docker,
// starts the instance there and checks it, and rolls back on a failure.
//
// Each container is carried whole: `docker commit` keeps its writable layer
// (a kind node's /etc/kubernetes and /kind), `docker save | docker load`
// takes the image across, each volume's content goes as a tar stream
// between two helper containers, and the container is created again from
// its own `docker inspect` with the same name, labels, privileges, mounts,
// published ports, networks and addresses. A kind node keeps its IP, which
// its certificates name, as its network is made with the same subnets. A
// bind mount of a path of the source machine becomes a volume filled with
// the path's content (as internal/hostmount does); a system path (/lib/modules,
// /dev, the Docker socket) stays the target host's own.
//
// Nothing is removed from the source until the caller has checked the
// instance on the target: Rollback removes what the carry made on the
// target and starts the source's containers again, and RemoveSource
// removes the source's once the move is done.

// DockerEndpoint is one Docker the carry reaches: Host is the DOCKER_HOST
// value, "" for this process's own environment.
type DockerEndpoint struct {
	Host string
	// Helper is an image there with tar, used to read and write volumes.
	Helper string
}

// CarryOptions say what to carry, from where to where.
type CarryOptions struct {
	Instance string
	// Clusters are the kind clusters whose nodes are the instance's.
	Clusters []string
	From, To DockerEndpoint
	// Progress receives each step in words a console can show.
	Progress func(step, message string)
	// Docker is the docker CLI; empty is "docker".
	Docker string
	// An empty To.Helper is the first carried container's image once it is
	// loaded: a kind control plane's, which has tar.
}

// Carry is one carry, its record of what it did on each side.
type Carry struct {
	o CarryOptions

	mu sync.Mutex
	// source
	containers []containerInfo
	wasRunning []string
	committed  []string // image refs committed on From
	volumes    []string // volumes on From that were carried
	// target
	madeContainers []string
	madeVolumes    []string
	madeNetworks   []string
	loadedImages   []string // image IDs loaded on To
	// Bytes is how much was copied, for the progress.
	bytes atomic.Int64
}

type containerInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Hostname   string            `json:"Hostname"`
		User       string            `json:"User"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
		Tty        bool              `json:"Tty"`
		OpenStdin  bool              `json:"OpenStdin"`
		WorkingDir string            `json:"WorkingDir"`
		StopSignal string            `json:"StopSignal"`
		Image      string            `json:"Image"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		Privileged    bool                     `json:"Privileged"`
		SecurityOpt   []string                 `json:"SecurityOpt"`
		Tmpfs         map[string]string        `json:"Tmpfs"`
		PortBindings  map[string][]portBinding `json:"PortBindings"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		CgroupnsMode string   `json:"CgroupnsMode"`
		Init         *bool    `json:"Init"`
		ShmSize      int64    `json:"ShmSize"`
		CapAdd       []string `json:"CapAdd"`
		ExtraHosts   []string `json:"ExtraHosts"`
		NetworkMode  string   `json:"NetworkMode"`
		ReadonlyRoot bool     `json:"ReadonlyRootfs"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAMConfig *struct {
				IPv4Address string `json:"IPv4Address"`
				IPv6Address string `json:"IPv6Address"`
			} `json:"IPAMConfig"`
			IPAddress         string   `json:"IPAddress"`
			GlobalIPv6Address string   `json:"GlobalIPv6Address"`
			Aliases           []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type networkInfo struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	IPAM       struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

func (c *Carry) progress(step, format string, args ...any) {
	if c.o.Progress != nil {
		c.o.Progress(step, fmt.Sprintf(format, args...))
	}
}

func (c *Carry) cmd(ctx context.Context, side DockerEndpoint, args ...string) *exec.Cmd {
	bin := c.o.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if side.Host != "" && (strings.HasPrefix(e, "DOCKER_HOST=") || strings.HasPrefix(e, "DOCKER_CONTEXT=")) {
			continue
		}
		env = append(env, e)
	}
	if side.Host != "" {
		env = append(env, "DOCKER_HOST="+side.Host)
	}
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func (c *Carry) run(ctx context.Context, side DockerEndpoint, args ...string) (string, error) {
	cmd := c.cmd(ctx, side, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// pipe runs from's command into to's, counting the bytes.
func (c *Carry) pipe(ctx context.Context, what string, from []string, to []string) error {
	src := c.cmd(ctx, c.o.From, from...)
	dst := c.cmd(ctx, c.o.To, to...)
	pr, pw := io.Pipe()
	var srcErr, dstErr bytes.Buffer
	src.Stdout, src.Stderr = &countingWriter{w: pw, n: &c.bytes}, &srcErr
	dst.Stdin, dst.Stderr = pr, &dstErr
	var dstOut bytes.Buffer
	dst.Stdout = &dstOut
	if err := dst.Start(); err != nil {
		return err
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				c.progress("copy", "Copying %s: %s so far", what, humanBytes(c.bytes.Load()))
			}
		}
	}()
	serr := src.Run()
	_ = pw.CloseWithError(serr)
	derr := dst.Wait()
	close(stop)
	if serr != nil {
		return fmt.Errorf("read %s: %v: %s", what, serr, strings.TrimSpace(srcErr.String()))
	}
	if derr != nil {
		return fmt.Errorf("write %s: %v: %s", what, derr, strings.TrimSpace(dstErr.String()))
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

// Bytes is how much the carry has copied.
func (c *Carry) Bytes() int64 { return c.bytes.Load() }

// systemBind is a bind mount of the host's own, which stays the target's.
func systemBind(src string) bool {
	for _, p := range []string{"/lib/modules", "/dev", "/sys", "/proc", "/run", "/var/run/docker.sock", "/etc/localtime"} {
		if src == p || strings.HasPrefix(src, p+"/") {
			return true
		}
	}
	return false
}

// StartCarry stops the instance's containers on From and carries them, with
// their volumes and networks, to To, created but not started. On an error
// it has rolled back already.
func StartCarry(ctx context.Context, o CarryOptions) (*Carry, error) {
	c := &Carry{o: o}
	if err := c.carry(ctx); err != nil {
		c.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}

func (c *Carry) list(ctx context.Context) ([]string, error) {
	filters := []string{"label=io.cloudburrow.instance=" + c.o.Instance, "label=cloudburrow.instance=" + c.o.Instance}
	for _, cl := range c.o.Clusters {
		filters = append(filters, "label=io.x-k8s.kind.cluster="+cl)
	}
	seen := map[string]bool{}
	var ids []string
	for _, f := range filters {
		out, err := c.run(ctx, c.o.From, "ps", "-a", "-q", "--no-trunc", "--filter", f)
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(out) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (c *Carry) carry(ctx context.Context) error {
	ids, err := c.list(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("instance %s has no containers on this Docker to move", c.o.Instance)
	}
	out, err := c.run(ctx, c.o.From, append([]string{"inspect"}, ids...)...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), &c.containers); err != nil {
		return fmt.Errorf("read the containers' configuration: %w", err)
	}
	// Nodes first: the kind control plane's image is To's helper.
	sort.SliceStable(c.containers, func(i, j int) bool {
		return c.containers[i].Config.Labels["io.x-k8s.kind.role"] == "control-plane" && c.containers[j].Config.Labels["io.x-k8s.kind.role"] != "control-plane"
	})
	for _, ct := range c.containers {
		if _, err := c.run(ctx, c.o.To, "container", "inspect", strings.TrimPrefix(ct.Name, "/")); err == nil {
			return fmt.Errorf("the target already has a container named %s; it would be replaced, so nothing was moved", strings.TrimPrefix(ct.Name, "/"))
		}
	}

	c.progress("stop", "Stopping %d containers of %s", len(c.containers), c.o.Instance)
	for _, ct := range c.containers {
		if ct.State.Running {
			c.wasRunning = append(c.wasRunning, ct.ID)
			if _, err := c.run(ctx, c.o.From, "stop", "-t", "30", ct.ID); err != nil {
				return err
			}
		}
	}

	if err := c.networks(ctx); err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	images := map[string]string{} // container ID -> image ref on both sides
	for _, ct := range c.containers {
		name := strings.TrimPrefix(ct.Name, "/")
		ref := "cloudburrow.local/move-" + sanitize(c.o.Instance) + "-" + sanitize(name) + ":" + stamp
		c.progress("image", "Saving %s's filesystem", name)
		if _, err := c.run(ctx, c.o.From, "commit", "--pause=false", ct.ID, ref); err != nil {
			return err
		}
		c.committed = append(c.committed, ref)
		c.progress("image", "Copying %s's image to the target", name)
		if err := c.pipe(ctx, name+"'s image", []string{"save", ref}, []string{"load", "-q"}); err != nil {
			return err
		}
		id, err := c.run(ctx, c.o.To, "image", "inspect", "--format", "{{.Id}}", ref)
		if err != nil {
			return err
		}
		c.loadedImages = append(c.loadedImages, id)
		images[ct.ID] = ref
		if c.o.To.Helper == "" {
			c.o.To.Helper = ref
		}
	}
	if err := c.volumesAcross(ctx); err != nil {
		return err
	}
	for _, ct := range c.containers {
		if err := c.create(ctx, ct, images[ct.ID]); err != nil {
			return err
		}
	}
	c.progress("carried", "Carried %d containers and %d volumes (%s)", len(c.containers), len(c.volumes), humanBytes(c.bytes.Load()))
	return nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// networks makes each network the containers join on To, with the same
// subnets, so each keeps its address.
func (c *Carry) networks(ctx context.Context) error {
	names := map[string]bool{}
	for _, ct := range c.containers {
		for n := range ct.NetworkSettings.Networks {
			if n != "bridge" && n != "host" && n != "none" {
				names[n] = true
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		var src []networkInfo
		out, err := c.run(ctx, c.o.From, "network", "inspect", n)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(out), &src); err != nil || len(src) != 1 {
			return fmt.Errorf("read network %s: %v", n, err)
		}
		want := src[0]
		if out, err := c.run(ctx, c.o.To, "network", "inspect", n); err == nil {
			var have []networkInfo
			if err := json.Unmarshal([]byte(out), &have); err != nil || len(have) != 1 {
				return fmt.Errorf("read the target's network %s: %v", n, err)
			}
			if subnets(have[0]) != subnets(want) {
				return fmt.Errorf("the target's network %s has subnets %s, not %s: the containers could not keep their addresses (a kind node's certificates name its address)",
					n, subnets(have[0]), subnets(want))
			}
			continue
		}
		args := []string{"network", "create", "--driver", want.Driver}
		if want.Internal {
			args = append(args, "--internal")
		}
		if want.EnableIPv6 {
			args = append(args, "--ipv6")
		}
		for _, cfg := range want.IPAM.Config {
			if cfg.Subnet != "" {
				args = append(args, "--subnet", cfg.Subnet)
			}
			if cfg.Gateway != "" {
				args = append(args, "--gateway", cfg.Gateway)
			}
		}
		for _, k := range sortedKeys(want.Labels) {
			args = append(args, "--label", k+"="+want.Labels[k])
		}
		for _, k := range sortedKeys(want.Options) {
			if strings.HasPrefix(k, "com.docker.network.") {
				args = append(args, "-o", k+"="+want.Options[k])
			}
		}
		c.progress("network", "Making network %s on the target (%s)", n, subnets(want))
		if _, err := c.run(ctx, c.o.To, append(args, n)...); err != nil {
			return err
		}
		c.madeNetworks = append(c.madeNetworks, n)
	}
	return nil
}

func subnets(n networkInfo) string {
	var s []string
	for _, cfg := range n.IPAM.Config {
		s = append(s, cfg.Subnet)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

// volumesAcross carries every volume the containers mount, and every
// volume labelled for the instance.
func (c *Carry) volumesAcross(ctx context.Context) error {
	set := map[string]bool{}
	var vols []string
	add := func(v string) {
		if v != "" && !set[v] {
			set[v] = true
			vols = append(vols, v)
		}
	}
	for _, ct := range c.containers {
		for _, m := range ct.Mounts {
			if m.Type == "volume" {
				add(m.Name)
			}
		}
	}
	for _, key := range []string{"io.cloudburrow.instance", "cloudburrow.instance"} {
		out, err := c.run(ctx, c.o.From, "volume", "ls", "-q", "--filter", "label="+key+"="+c.o.Instance)
		if err != nil {
			return err
		}
		for _, v := range strings.Fields(out) {
			add(v)
		}
	}
	helper := c.o.From.Helper
	for _, v := range vols {
		if _, err := c.run(ctx, c.o.To, "volume", "inspect", v); err == nil {
			return fmt.Errorf("the target already has a volume named %s; it would be overwritten, so nothing was moved", v)
		}
		var info []struct {
			Labels map[string]string `json:"Labels"`
			Driver string            `json:"Driver"`
		}
		out, err := c.run(ctx, c.o.From, "volume", "inspect", v)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(out), &info); err != nil || len(info) != 1 {
			return fmt.Errorf("read volume %s: %v", v, err)
		}
		args := []string{"volume", "create"}
		for _, k := range sortedKeys(info[0].Labels) {
			args = append(args, "--label", k+"="+info[0].Labels[k])
		}
		if _, err := c.run(ctx, c.o.To, append(args, v)...); err != nil {
			return err
		}
		c.madeVolumes = append(c.madeVolumes, v)
		c.volumes = append(c.volumes, v)
		c.progress("volume", "Copying volume %s", short(v))
		if err := c.pipe(ctx, "volume "+short(v),
			[]string{"run", "--rm", "--network", "none", "-v", v + ":/v:ro", "--entrypoint", "tar", helper, "-C", "/v", "-cf", "-", "."},
			[]string{"run", "--rm", "-i", "--network", "none", "-v", v + ":/v", "--entrypoint", "tar", c.o.To.Helper, "-C", "/v", "-xpf", "-"}); err != nil {
			return err
		}
	}
	return nil
}

func short(v string) string {
	if len(v) == 64 {
		return v[:12]
	}
	return v
}

// create makes the container again on To from its own configuration.
func (c *Carry) create(ctx context.Context, ct containerInfo, image string) error {
	name := strings.TrimPrefix(ct.Name, "/")
	args := []string{"create", "--name", name}
	if ct.Config.Hostname != "" && ct.Config.Hostname != ct.ID[:12] {
		args = append(args, "--hostname", ct.Config.Hostname)
	}
	for _, k := range sortedKeys(ct.Config.Labels) {
		args = append(args, "--label", k+"="+ct.Config.Labels[k])
	}
	if ct.Config.User != "" {
		args = append(args, "--user", ct.Config.User)
	}
	if ct.Config.Tty {
		args = append(args, "--tty")
	}
	if ct.Config.OpenStdin {
		args = append(args, "--interactive")
	}
	if ct.Config.WorkingDir != "" {
		args = append(args, "--workdir", ct.Config.WorkingDir)
	}
	if ct.Config.StopSignal != "" {
		args = append(args, "--stop-signal", ct.Config.StopSignal)
	}
	hc := ct.HostConfig
	if hc.Privileged {
		args = append(args, "--privileged")
	}
	for _, o := range hc.SecurityOpt {
		args = append(args, "--security-opt", o)
	}
	for _, k := range sortedKeys(hc.Tmpfs) {
		spec := k
		if hc.Tmpfs[k] != "" {
			spec += ":" + hc.Tmpfs[k]
		}
		args = append(args, "--tmpfs", spec)
	}
	ports := make([]string, 0, len(hc.PortBindings))
	for p := range hc.PortBindings {
		ports = append(ports, p)
	}
	sort.Strings(ports)
	for _, p := range ports {
		for _, b := range hc.PortBindings[p] {
			spec := p
			if b.HostPort != "" || b.HostIP != "" {
				spec = b.HostPort + ":" + p
				if b.HostIP != "" {
					spec = b.HostIP + ":" + spec
				}
			}
			args = append(args, "--publish", spec)
		}
	}
	if r := hc.RestartPolicy; r.Name != "" && r.Name != "no" {
		pol := r.Name
		if r.Name == "on-failure" && r.MaximumRetryCount > 0 {
			pol += ":" + strconv.Itoa(r.MaximumRetryCount)
		}
		args = append(args, "--restart", pol)
	}
	if hc.CgroupnsMode != "" {
		args = append(args, "--cgroupns", hc.CgroupnsMode)
	}
	if hc.Init != nil && *hc.Init {
		args = append(args, "--init")
	}
	if hc.ShmSize > 0 && hc.ShmSize != 64<<20 {
		args = append(args, "--shm-size", strconv.FormatInt(hc.ShmSize, 10))
	}
	for _, cp := range hc.CapAdd {
		args = append(args, "--cap-add", cp)
	}
	for _, h := range hc.ExtraHosts {
		args = append(args, "--add-host", h)
	}
	if hc.ReadonlyRoot {
		args = append(args, "--read-only")
	}
	for _, m := range ct.Mounts {
		switch m.Type {
		case "volume":
			spec := "type=volume,src=" + m.Name + ",dst=" + m.Destination
			if !m.RW {
				spec += ",readonly"
			}
			args = append(args, "--mount", spec)
		case "bind":
			if systemBind(m.Source) {
				spec := "type=bind,src=" + m.Source + ",dst=" + m.Destination
				if !m.RW {
					spec += ",readonly"
				}
				args = append(args, "--mount", spec)
				continue
			}
			v, err := c.bindVolume(ctx, m.Source)
			if err != nil {
				return err
			}
			spec := "type=volume,src=" + v + ",dst=" + m.Destination
			if !m.RW {
				spec += ",readonly"
			}
			args = append(args, "--mount", spec)
		}
	}
	// The first network at create, the rest connected after, each with its
	// address.
	nets := make([]string, 0, len(ct.NetworkSettings.Networks))
	for n := range ct.NetworkSettings.Networks {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	if mode := hc.NetworkMode; mode != "" && mode != "default" && !ct.hasNetwork(mode) {
		args = append(args, "--network", mode)
		nets = nil
	}
	netArgs := func(n string) []string {
		e := ct.NetworkSettings.Networks[n]
		var a []string
		ip4, ip6 := e.IPAddress, e.GlobalIPv6Address
		if e.IPAMConfig != nil {
			if e.IPAMConfig.IPv4Address != "" {
				ip4 = e.IPAMConfig.IPv4Address
			}
			if e.IPAMConfig.IPv6Address != "" {
				ip6 = e.IPAMConfig.IPv6Address
			}
		}
		if n != "bridge" {
			if ip4 != "" {
				a = append(a, "--ip", ip4)
			}
			if ip6 != "" {
				a = append(a, "--ip6", ip6)
			}
			for _, al := range e.Aliases {
				if al != name && al != ct.ID[:12] {
					a = append(a, "--alias", al)
				}
			}
		}
		return a
	}
	if len(nets) > 0 {
		args = append(args, "--network", nets[0])
		args = append(args, netArgs(nets[0])...)
	}
	for _, e := range ct.Config.Env {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	id, err := c.run(ctx, c.o.To, args...)
	if err != nil {
		return fmt.Errorf("create %s on the target: %w", name, err)
	}
	c.madeContainers = append(c.madeContainers, id)
	for _, n := range nets[min(1, len(nets)):] {
		a := append([]string{"network", "connect"}, netArgs(n)...)
		if _, err := c.run(ctx, c.o.To, append(a, n, id)...); err != nil {
			return fmt.Errorf("connect %s to %s on the target: %w", name, n, err)
		}
	}
	c.progress("container", "Made %s on the target", name)
	return nil
}

func (ct containerInfo) hasNetwork(n string) bool {
	_, ok := ct.NetworkSettings.Networks[n]
	return ok
}

// bindVolume carries a path of the source machine into a volume on To.
func (c *Carry) bindVolume(ctx context.Context, src string) (string, error) {
	sum := sha256.Sum256([]byte(c.o.Instance + "\x00" + src))
	v := "cb-mount-" + sanitize(c.o.Instance) + "-moved-" + hex.EncodeToString(sum[:8])
	if _, err := c.run(ctx, c.o.To, "volume", "inspect", v); err == nil {
		return v, nil
	}
	if _, err := c.run(ctx, c.o.To, "volume", "create", "--label", "io.cloudburrow.instance="+c.o.Instance, v); err != nil {
		return "", err
	}
	c.madeVolumes = append(c.madeVolumes, v)
	c.progress("volume", "Copying %s of this machine into volume %s", src, v)
	cmd := c.cmd(ctx, c.o.To, "run", "--rm", "-i", "--network", "none", "-v", v+":/v", "--entrypoint", "tar", c.o.To.Helper, "-C", "/v", "-xpf", "-")
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return "", err
	}
	werr := tarPath(&countingWriter{w: pw, n: &c.bytes}, src)
	_ = pw.CloseWithError(werr)
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("copy %s: %v: %s", src, err, strings.TrimSpace(errOut.String()))
	}
	return v, werr
}

// tarPath writes a directory's tree, or a file, as a tar.
func tarPath(w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	base := root
	if !info.IsDir() {
		base = filepath.Dir(root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// StartTarget starts the carried containers on To that ran on From.
func (c *Carry) StartTarget(ctx context.Context) error {
	running := map[string]bool{}
	for _, id := range c.wasRunning {
		running[id] = true
	}
	for i, ct := range c.containers {
		if !running[ct.ID] || i >= len(c.madeContainers) {
			continue
		}
		if _, err := c.run(ctx, c.o.To, "start", c.madeContainers[i]); err != nil {
			return err
		}
	}
	return nil
}

// Rollback removes everything the carry made on To, and starts again the
// containers it stopped on From.
func (c *Carry) Rollback(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progress("rollback", "Rolling back: removing what was made on the target")
	for _, id := range c.madeContainers {
		_, _ = c.run(ctx, c.o.To, "rm", "-f", "-v", id)
	}
	for _, v := range c.madeVolumes {
		_, _ = c.run(ctx, c.o.To, "volume", "rm", "-f", v)
	}
	for _, id := range c.loadedImages {
		_, _ = c.run(ctx, c.o.To, "rmi", "-f", id)
	}
	for _, n := range c.madeNetworks {
		_, _ = c.run(ctx, c.o.To, "network", "rm", n)
	}
	for _, ref := range c.committed {
		_, _ = c.run(ctx, c.o.From, "rmi", ref)
	}
	c.madeContainers, c.madeVolumes, c.loadedImages, c.madeNetworks, c.committed = nil, nil, nil, nil, nil
	for _, id := range c.wasRunning {
		_, _ = c.run(ctx, c.o.From, "start", id)
	}
	c.progress("rollback", "Rolled back: the instance's containers are on this machine again")
}

// RemoveSource removes the carried containers and volumes from From, and
// the images the carry committed there, once the instance runs on To. The
// networks stay (kind's is shared by every cluster on that Docker).
func (c *Carry) RemoveSource(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, ct := range c.containers {
		if _, err := c.run(ctx, c.o.From, "rm", "-f", "-v", ct.ID); err != nil {
			errs = append(errs, err)
		}
	}
	for _, v := range c.volumes {
		if _, err := c.run(ctx, c.o.From, "volume", "rm", "-f", v); err != nil {
			errs = append(errs, err)
		}
	}
	for _, ref := range c.committed {
		_, _ = c.run(ctx, c.o.From, "rmi", ref)
	}
	c.committed = nil
	return errors.Join(errs...)
}

// Containers is the names of the carried containers.
func (c *Carry) Containers() []string {
	var n []string
	for _, ct := range c.containers {
		n = append(n, strings.TrimPrefix(ct.Name, "/"))
	}
	return n
}
