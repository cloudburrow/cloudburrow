package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// Moving an instance (#2209): `cloudburrow cloud move <instance> --to <vm>`
// and the console's Move. The instance's Docker state (its kind nodes,
// their volumes, and every container and volume labelled for it) goes from
// this machine's Docker to the VM's; the state directory, the APIs and the
// console stay here, as with any remote compute. The steps:
//
//  1. stop the instance's `up` (its arguments are in the runtime file);
//  2. connect to the VM, check it, and carry the containers, volumes and
//     networks (remotecompute.StartCarry), nothing removed here yet;
//  3. record the VM as the instance's compute, and start `up` again with
//     its own arguments: it runs on the VM now;
//  4. check it: `up` reports ready, and the kind node runs on the VM;
//  5. remove the instance's containers and volumes from this machine.
//
// A failure at 2 to 4 rolls back: whatever was made on the VM is removed,
// the record goes, and the instance starts again here as it was.

// moveTimeout bounds a move; the copy of a large cluster over a slow link
// is most of it.
const moveTimeout = 3 * time.Hour

// moveStep is one line of a move's progress.
type moveStep func(step, message string)

// moveInstance moves cfg's instance onto to: a VM's name in the
// provisioner, or ssh://user@host for a host of the user's own.
func moveInstance(ctx context.Context, cfg config.Config, m *byoc.Manager, to string, step moveStep, out io.Writer) (err error) {
	if step == nil {
		step = func(string, string) {}
	}
	if rec, had, err := remotecompute.LoadRecord(cfg.InstanceDir()); err != nil {
		return err
	} else if had {
		return fmt.Errorf("instance %s's compute is %s already; a move carries an instance from this machine's Docker", cfg.Name, rec.Target)
	}
	if !localClusterExists(ctx, cfg) {
		return fmt.Errorf("instance %s has no cluster on this machine's Docker to move; start it on the VM with `cloudburrow up`", cfg.Name)
	}
	opts := remotecompute.Options{StateDir: cfg.InstanceDir(), Instance: cfg.Name}
	rec := remotecompute.Record{Created: time.Now().UTC()}
	machine := ""
	if strings.HasPrefix(to, "ssh://") {
		t, err := remotecompute.ParseTarget(to)
		if err != nil {
			return err
		}
		opts.Target = t
	} else {
		if m == nil {
			return errors.New("no provisioner to read the VM from")
		}
		mc, err := m.Machine(to)
		if err != nil {
			return fmt.Errorf("VM %q: %w (create it with `cloudburrow cloud create`, or in the console's Compute capacity page)", to, err)
		}
		if mc.State != byoc.Running {
			return fmt.Errorf("VM %s is %s; start it first", to, mc.State)
		}
		if mc.UsedBy != "" && mc.UsedBy != cfg.Name {
			return fmt.Errorf("VM %s is instance %s's compute already", to, mc.UsedBy)
		}
		if mc.Instance != cfg.Name {
			if _, had, _ := remotecompute.LoadRecord(filepath.Join(cfg.StateDir, mc.Instance)); had {
				return fmt.Errorf("VM %s is instance %s's compute already", to, mc.Instance)
			}
		}
		vm, err := m.RemoteTarget(to)
		if err != nil {
			return err
		}
		opts.Target = remotecompute.Target{User: vm.User, Host: vm.Host, Port: vm.Port}
		if opts.Target.Port == 0 {
			opts.Target.Port = 22
		}
		opts.Key, opts.HostKey = remotecompute.PEMKey(vm.PrivateKey), vm.HostKey
		rec.Provisioned, machine = true, to
		if to != cfg.Name {
			rec.Machine = to
		}
	}
	rec.Target = opts.Target

	// 1. The instance's up, and how it was started.
	info, wasUp := running(cfg)
	upArgs, offline := info.Args, info.Offline
	if !wasUp || len(upArgs) == 0 {
		upArgs = []string{"--name", cfg.Name, "--state-dir", cfg.StateDir}
	}
	if wasUp {
		step("stop", "Stopping "+cfg.Name+" on this machine")
		if err := stopRunning(cfg, out); err != nil {
			return err
		}
	}
	restartHere := func() {
		if wasUp {
			step("rollback", "Starting "+cfg.Name+" on this machine again")
			if err := runDetached(upArgs, 20*time.Minute, offline, out, out); err != nil {
				step("rollback", "Could not start "+cfg.Name+" here again: "+err.Error())
			}
		}
	}

	// 2. The VM, and the carry.
	step("connect", "Connecting to "+opts.Target.String())
	opts.Progress = func(e remotecompute.Event) {
		if e.Stage == remotecompute.StagePrerequisites || e.Stage == remotecompute.StageConnected || e.Stage == remotecompute.StageFailed {
			msg := e.Message
			if e.Error != "" {
				msg += ": " + e.Error
			}
			step("connect", msg)
		}
	}
	s, err := remotecompute.Connect(ctx, opts)
	if err != nil {
		restartHere()
		return fmt.Errorf("connect to %s: %w", opts.Target, err)
	}
	defer s.Close()
	rec.Host = s.Status().Host
	carry, err := remotecompute.StartCarry(ctx, remotecompute.CarryOptions{
		Instance: cfg.Name, Clusters: instanceClusters(ctx, cfg),
		From:     remotecompute.DockerEndpoint{Helper: cfg.Cluster.NodeImage},
		To:       remotecompute.DockerEndpoint{Host: s.DockerHost()},
		Progress: step,
	})
	if err != nil {
		restartHere()
		return fmt.Errorf("carry %s's state to %s: %w", cfg.Name, opts.Target, err)
	}
	rollback := func(cause error) error {
		step("rollback", "The move failed: "+cause.Error())
		if wasUpThere, ok := running(cfg); ok && wasUpThere.PID > 0 {
			_ = stopRunning(cfg, out)
		}
		_ = remotecompute.RemoveRecord(cfg.InstanceDir())
		if machine != "" {
			_ = m.SetUsedBy(machine, "")
		}
		carry.Rollback(context.WithoutCancel(ctx))
		restartHere()
		return cause
	}

	// 3. The VM is the instance's compute; up runs there.
	if err := remotecompute.SaveRecord(cfg.InstanceDir(), rec); err != nil {
		return rollback(err)
	}
	if machine != "" && machine != cfg.Name {
		if err := m.SetUsedBy(machine, cfg.Name); err != nil {
			return rollback(err)
		}
	}
	step("start", "Starting "+cfg.Name+" on "+opts.Target.String())
	if err := runDetached(upArgs, 30*time.Minute, offline, out, out); err != nil {
		return rollback(fmt.Errorf("start %s on %s: %w", cfg.Name, opts.Target, err))
	}

	// 4. Checked: up is ready (runDetached waited for it), and the
	// cluster's nodes run on the VM.
	step("verify", "Checking that "+cfg.Name+"'s cluster runs on "+opts.Target.String())
	for _, name := range carry.Containers() {
		if !strings.HasPrefix(name, cfg.ClusterName()+"-") {
			continue
		}
		st, err := s.Run(ctx, "docker inspect --format '{{.State.Running}}' "+shellWord(name))
		if err != nil || strings.TrimSpace(st) != "true" {
			return rollback(fmt.Errorf("node %s is not running on %s after the move (%v %s)", name, opts.Target, err, strings.TrimSpace(st)))
		}
	}

	// 5. Done: this machine's copy goes.
	step("cleanup", "Removing "+cfg.Name+"'s containers and volumes from this machine")
	if err := carry.RemoveSource(context.WithoutCancel(ctx)); err != nil {
		step("cleanup", "Some of this machine's copy could not be removed: "+err.Error())
	}
	step("done", fmt.Sprintf("%s runs on %s; %s carried", cfg.Name, opts.Target, humanSize(carry.Bytes())))
	return nil
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

// instanceClusters is the instance's kind clusters on this machine's
// Docker: its own, and the Kubernetes Engine API's extra clusters, named
// after it, that are not another instance's own cluster.
func instanceClusters(ctx context.Context, cfg config.Config) []string {
	out := []string{cfg.ClusterName()}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	names, err := kindClusters(ctx)
	if err != nil {
		return out
	}
	others := map[string]bool{}
	if entries, err := os.ReadDir(cfg.StateDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != cfg.Name {
				c := cfg
				c.Name = e.Name()
				others[c.ClusterName()] = true
			}
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, cfg.ClusterName()+"-") && !others[n] {
			out = append(out, n)
		}
	}
	return out
}

// move is `cloudburrow cloud move <instance> --to <vm>`.
func (c cloudCmd) move(ctx context.Context) error {
	to := c.flags["to"]
	if to == "" {
		return errors.New("usage: cloudburrow cloud move <instance> --to <vm>|ssh://user@host")
	}
	cfg := c.cfg
	if len(c.args) == 1 {
		cfg.Name = c.args[0]
	} else if len(c.args) > 1 {
		return errors.New("usage: cloudburrow cloud move <instance> --to <vm>|ssh://user@host")
	}
	fmt.Fprintf(c.stdout, "Moving %s to %s: it stops, its cluster's state is copied, and it starts there.\n", cfg.Name, to)
	ok, err := c.confirm("Move it?")
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintln(c.stdout, "Nothing was moved.")
		}
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
	// The steps go to the terminal and to the instance's move.json, which
	// the console follows (a console-started move names it).
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: cfg.Name, To: to, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	if path := os.Getenv(moveProgressEnv); path != "" {
		p.path = path
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &p.st)
		}
	}
	err = moveInstance(ctx, cfg, c.m, to, func(step, msg string) {
		fmt.Fprintf(c.stdout, "%-10s %s\n", step, msg)
		p.step(step, msg)
	}, c.stdout)
	p.finish(err)
	return err
}

// moveFile is where a move writes its progress, in the instance's state, so
// the console that the move restarts follows it.
func moveFile(cfg config.Config) string { return filepath.Join(cfg.InstanceDir(), "move.json") }

// moveProgress records a move's steps in moveFile as they happen.
type moveProgress struct {
	path string
	st   console.MoveStatus
}

func (p *moveProgress) step(step, msg string) {
	p.st.Steps = append(p.st.Steps, console.MoveStep{Time: time.Now().UTC(), Step: step, Message: msg})
	if step == "rollback" {
		p.st.RolledBack = true
	}
	p.write()
}

func (p *moveProgress) finish(err error) {
	p.st.Done = true
	if err != nil {
		p.st.Error = err.Error()
	}
	p.write()
}

func (p *moveProgress) write() {
	b, err := json.MarshalIndent(p.st, "", "  ")
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// consoleMover is the console's Move: `cloudburrow cloud move` in a process
// of its own, which outlives the `up` it stops.
type consoleMover struct{ cfg config.Config }

func (m consoleMover) StartMove(instance, vm string) error {
	cfg := m.cfg
	cfg.Name = instance
	if st, ok := m.MoveStatus(instance); ok && !st.Done {
		return fmt.Errorf("%s is being moved to %s already", instance, st.To)
	}
	if rec, had, err := remotecompute.LoadRecord(cfg.InstanceDir()); err != nil {
		return err
	} else if had {
		return fmt.Errorf("%s's compute is %s already", instance, rec.Target)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: instance, To: vm, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	p.step("queued", "Starting the move of "+instance+" to "+vm)
	logf, err := os.OpenFile(filepath.Join(cfg.InstanceDir(), "move.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	child := exec.Command(exe, "cloud", "move", instance, "--to", vm, "--yes", "--name", instance, "--state-dir", cfg.StateDir)
	child.Env = append(os.Environ(), moveProgressEnv+"="+p.path)
	child.Stdout, child.Stderr = logf, logf
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		p.finish(err)
		return fmt.Errorf("start the move: %w", err)
	}
	go func() { _ = child.Wait() }()
	return nil
}

func (m consoleMover) MoveStatus(instance string) (console.MoveStatus, bool) {
	cfg := m.cfg
	cfg.Name = instance
	b, err := os.ReadFile(moveFile(cfg))
	if err != nil {
		return console.MoveStatus{}, false
	}
	var st console.MoveStatus
	if json.Unmarshal(b, &st) != nil {
		return console.MoveStatus{}, false
	}
	return st, true
}

// moveProgressEnv names the progress file a console-started move writes.
const moveProgressEnv = "CLOUDBURROW_MOVE_PROGRESS"
