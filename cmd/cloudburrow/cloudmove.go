package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// Moving an instance (#2209): `cloudburrow cloud move <instance> --to
// <vm>|ssh://user@host|local` and the console's Move. The instance's Docker
// state (its kind nodes, their volumes, and every container and volume
// labelled for it) goes from the Docker it runs on now (this machine's, a
// VM's, or a host's of the user's own) to another: a VM in the same or
// another cloud (AWS to Azure), a host, or this machine again. The state
// directory, the APIs and the console stay here, as with any remote
// compute. The steps:
//
//  1. stop the instance's `up` (its arguments are in the runtime file);
//  2. connect to both ends, check them, and carry the containers, volumes
//     and networks (remotecompute.StartCarry), nothing removed yet;
//  3. record the destination as the instance's compute (or forget the
//     record, for this machine), and start `up` again with its own
//     arguments: it runs there now;
//  4. check it: `up` reports ready, and the kind node runs there;
//  5. remove the instance's containers and volumes from the source.
//
// Each step is written to the instance's move journal (move-journal.json,
// with the carry's own in move-carry.json) before the next starts, so a
// move that was interrupted (the process killed, the laptop asleep, ^C)
// resumes from where it stopped (`cloud move <instance> --resume`, the
// console's Resume), or is undone (`--rollback`, Roll back). A failure at 2
// to 4 rolls back by itself: whatever was made at the destination is
// removed, the source's record comes back, and the instance starts again
// where it ran, as it was.

// moveTimeout bounds a move; the copy of a large cluster over a slow link
// is most of it.
const moveTimeout = 3 * time.Hour

// moveStep is one line of a move's progress.
type moveStep func(step, message string)

// moveToLocal is --to's name for this machine's own Docker.
const moveToLocal = "local"

// The move's phases, in order, as its journal records them.
const (
	phaseCarrying = "carrying" // the up is stopped; the carry runs
	phaseCarried  = "carried"  // everything is at the destination, not started
	phaseRecorded = "recorded" // the destination is the instance's compute
	phaseStarted  = "started"  // up runs at the destination and is checked
)

// moveJournal is move-journal.json: what a resume or a rollback needs.
type moveJournal struct {
	Instance string    `json:"instance"`
	To       string    `json:"to"`
	Phase    string    `json:"phase"`
	Started  time.Time `json:"started"`
	WasUp    bool      `json:"was_up"`
	UpArgs   []string  `json:"up_args"`
	Offline  bool      `json:"offline,omitempty"`
	// From is the source's record; nil is this machine's Docker.
	From *remotecompute.Record `json:"from,omitempty"`
	// Dest is the destination's record; nil is this machine's Docker.
	Dest *remotecompute.Record `json:"dest,omitempty"`
	// FromMachine and DestMachine are the provisioner's VMs at each end.
	FromMachine string `json:"from_machine,omitempty"`
	DestMachine string `json:"dest_machine,omitempty"`
}

func moveJournalPath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "move-journal.json")
}
func moveCarryPath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "move-carry.json")
}

func loadMoveJournal(cfg config.Config) (moveJournal, bool, error) {
	b, err := os.ReadFile(moveJournalPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return moveJournal{}, false, nil
	}
	if err != nil {
		return moveJournal{}, false, err
	}
	var j moveJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return moveJournal{}, false, fmt.Errorf("read %s: %w", moveJournalPath(cfg), err)
	}
	return j, true, nil
}

func saveMoveJournal(cfg config.Config, j moveJournal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := moveJournalPath(cfg) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, moveJournalPath(cfg))
}

// pendingMove is the refusal of an up, or a second move, while a move is
// unfinished and before its destination is the instance's compute.
func pendingMove(cfg config.Config) error {
	j, ok, err := loadMoveJournal(cfg)
	if err != nil || !ok {
		return err
	}
	if j.Phase == phaseCarrying || j.Phase == phaseCarried {
		return fmt.Errorf("instance %s's move to %s did not finish: resume it with `cloudburrow cloud move %s --resume`, or undo it with `--rollback` (or Resume and Roll back in the console's Compute capacity)",
			cfg.Name, j.To, cfg.Name)
	}
	return nil
}

// moveEnd is one end of a move: this machine's Docker, a host, or a VM.
type moveEnd struct {
	local   bool
	opts    remotecompute.Options
	machine string
	rec     remotecompute.Record
}

func (e moveEnd) String() string {
	if e.local {
		return "this machine"
	}
	if e.machine != "" {
		return e.machine
	}
	return e.opts.Target.String()
}

// sameEnd reports whether a and b are the same Docker.
func sameEnd(a, b moveEnd) bool {
	if a.local || b.local {
		return a.local == b.local
	}
	if a.machine != "" || b.machine != "" {
		return a.machine == b.machine
	}
	norm := func(t remotecompute.Target) remotecompute.Target {
		if t.Port == 0 {
			t.Port = 22
		}
		return t
	}
	return norm(a.opts.Target) == norm(b.opts.Target)
}

// endFor resolves a move's destination name: local, ssh://user@host, or a
// VM's name in the provisioner.
func endFor(ctx context.Context, cfg config.Config, m *byoc.Manager, to string) (moveEnd, error) {
	if to == moveToLocal {
		return moveEnd{local: true}, nil
	}
	e := moveEnd{opts: remotecompute.Options{StateDir: cfg.InstanceDir(), Instance: cfg.Name, SSHConfig: byocSSHConfig()}}
	e.rec = remotecompute.Record{Created: time.Now().UTC()}
	if strings.HasPrefix(to, "ssh://") {
		t, err := remotecompute.ParseTarget(to)
		if err != nil {
			return moveEnd{}, err
		}
		e.opts.Target, e.rec.Target = t, t
		return e, nil
	}
	if m == nil {
		return moveEnd{}, errors.New("no provisioner to read the VM from")
	}
	if _, err := m.Machine(to); err != nil {
		return moveEnd{}, fmt.Errorf("VM %q: %w (create it with `cloudburrow cloud create`, or in the console's Compute capacity page)", to, err)
	}
	e.machine = to
	e.rec.Provisioned = true
	if to != cfg.Name {
		e.rec.Machine = to
	}
	return e, nil
}

// endOf is where the instance runs now, from its record.
func endOf(cfg config.Config, rec *remotecompute.Record) moveEnd {
	if rec == nil {
		return moveEnd{local: true}
	}
	e := moveEnd{opts: remotecompute.Options{StateDir: cfg.InstanceDir(), Instance: cfg.Name, Target: rec.Target, SSHConfig: byocSSHConfig()}, rec: *rec}
	if rec.Provisioned {
		e.machine = rec.MachineName(cfg.Name)
	}
	return e
}

// checkVM refuses a destination VM that cannot take the instance.
func checkVM(cfg config.Config, m *byoc.Manager, name string) error {
	mc, err := m.Machine(name)
	if err != nil {
		return err
	}
	if mc.State != byoc.Running {
		return fmt.Errorf("VM %s is %s; start it first", name, mc.State)
	}
	if mc.UsedBy != "" && mc.UsedBy != cfg.Name {
		return fmt.Errorf("VM %s is instance %s's compute already", name, mc.UsedBy)
	}
	if mc.Instance != cfg.Name && mc.UsedBy == "" {
		if _, had, _ := remotecompute.LoadRecord(filepath.Join(cfg.StateDir, mc.Instance)); had && !neverStarted(cfg, mc.Instance) {
			return fmt.Errorf("VM %s is instance %s's compute already", name, mc.Instance)
		}
	}
	return nil
}

// neverStarted reports whether instance has never run: Create gave its VM
// to an instance of the VM's own name, which no up has started (no up.json,
// up.log or kubeconfig), so the VM is free for a move.
func neverStarted(cfg config.Config, instance string) bool {
	c := cfg
	c.Name = instance
	if _, up := running(c); up {
		return false
	}
	for _, f := range []string{"up.json", "up.log", "kubeconfig"} {
		if _, err := os.Stat(filepath.Join(c.InstanceDir(), f)); err == nil {
			return false
		}
	}
	return true
}

// open connects to a remote end; a local one has nothing to open.
func (e *moveEnd) open(ctx context.Context, m *byoc.Manager, step moveStep) (*remotecompute.Session, remotecompute.DockerEndpoint, error) {
	if e.local {
		return nil, remotecompute.DockerEndpoint{}, nil
	}
	if e.machine != "" {
		vm, err := m.RemoteTarget(e.machine)
		if err != nil {
			return nil, remotecompute.DockerEndpoint{}, err
		}
		e.opts.Target = remotecompute.Target{User: vm.User, Host: vm.Host, Port: vm.Port}
		if e.opts.Target.Port == 0 {
			e.opts.Target.Port = 22
		}
		e.opts.Key, e.opts.HostKey = remotecompute.PEMKey(vm.PrivateKey), vm.HostKey
		// SSM mode: the target is this process's Session Manager tunnel;
		// SSH mode: the firewall follows this machine's address.
		e.opts.BeforeConnect = vm.BeforeConnect
		e.rec.Via = vm.Via
		e.rec.Target = e.opts.Target
	}
	step("connect", "Connecting to "+e.String()+" ("+e.opts.Target.String()+")")
	o := e.opts
	o.Progress = func(ev remotecompute.Event) {
		if ev.Stage == remotecompute.StagePrerequisites || ev.Stage == remotecompute.StageConnected || ev.Stage == remotecompute.StageFailed {
			msg := ev.Message
			if ev.Error != "" {
				msg += ": " + ev.Error
			}
			step("connect", msg)
		}
	}
	s, err := remotecompute.Connect(ctx, o)
	if err != nil {
		return nil, remotecompute.DockerEndpoint{}, fmt.Errorf("connect to %s: %w", e, err)
	}
	e.rec.Host = s.Status().Host
	return s, remotecompute.DockerEndpoint{Host: s.DockerHost()}, nil
}

// withoutCompute is up's arguments less --compute: after a move the record
// says where the compute is.
func withoutCompute(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--compute" || a == "-compute" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--compute=") || strings.HasPrefix(a, "-compute=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// moveInstance moves cfg's instance onto to: a VM's name in the
// provisioner, ssh://user@host for a host of the user's own, or local for
// this machine's Docker.
func moveInstance(ctx context.Context, cfg config.Config, m *byoc.Manager, to string, step moveStep, out io.Writer) (err error) {
	if step == nil {
		step = func(string, string) {}
	}
	if _, pending, err := loadMoveJournal(cfg); err != nil {
		return err
	} else if pending {
		return fmt.Errorf("instance %s has an unfinished move: resume it with `cloudburrow cloud move %s --resume`, or undo it with `--rollback`", cfg.Name, cfg.Name)
	}
	var from *remotecompute.Record
	if rec, had, err := remotecompute.LoadRecord(cfg.InstanceDir()); err != nil {
		return err
	} else if had {
		from = &rec
	} else if !localClusterExists(ctx, cfg) {
		return fmt.Errorf("instance %s has no cluster on this machine's Docker to move; start it with `cloudburrow up`", cfg.Name)
	}
	src := endOf(cfg, from)
	if src.machine != "" && m == nil {
		return errors.New("no provisioner to read the instance's VM from")
	}
	dst, err := endFor(ctx, cfg, m, to)
	if err != nil {
		return err
	}
	if sameEnd(src, dst) {
		return fmt.Errorf("instance %s runs on %s already", cfg.Name, src)
	}
	if dst.machine != "" {
		if err := checkVM(cfg, m, dst.machine); err != nil {
			return err
		}
	}
	if src.machine != "" {
		if mc, err := m.Machine(src.machine); err == nil && mc.State != byoc.Running {
			return fmt.Errorf("instance %s's VM %s is %s; start it first: its cluster's state is on its disk", cfg.Name, src.machine, mc.State)
		}
	}

	// 1. The instance's up, and how it was started.
	info, wasUp := running(cfg)
	j := moveJournal{Instance: cfg.Name, To: to, Phase: phaseCarrying, Started: time.Now().UTC(), WasUp: wasUp,
		UpArgs: withoutCompute(info.Args), Offline: info.Offline, From: from, FromMachine: src.machine, DestMachine: dst.machine}
	if !wasUp || len(info.Args) == 0 {
		j.UpArgs = []string{"--name", cfg.Name, "--state-dir", cfg.StateDir}
	}
	if !dst.local {
		r := dst.rec
		j.Dest = &r
	}
	if err := saveMoveJournal(cfg, j); err != nil {
		return err
	}
	if wasUp {
		step("stop", "Stopping "+cfg.Name+" on "+src.String())
		if err := stopRunning(cfg, out); err != nil {
			_ = os.Remove(moveJournalPath(cfg))
			return err
		}
	}
	return runMove(ctx, cfg, m, j, src, dst, false, step, out)
}

// resumeMove continues the instance's unfinished move from its journal.
func resumeMove(ctx context.Context, cfg config.Config, m *byoc.Manager, step moveStep, out io.Writer) error {
	j, ok, err := loadMoveJournal(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("instance %s has no unfinished move to resume", cfg.Name)
	}
	src, dst, err := journalEnds(ctx, cfg, m, j)
	if err != nil {
		return err
	}
	step("resume", fmt.Sprintf("Resuming the move of %s from %s to %s (it had reached %q)", cfg.Name, src, dst, j.Phase))
	if info, up := running(cfg); up && info.PID > 0 && (j.Phase == phaseCarrying || j.Phase == phaseCarried) {
		_ = stopRunning(cfg, out)
	}
	return runMove(ctx, cfg, m, j, src, dst, true, step, out)
}

// rollbackMove undoes the instance's unfinished move from its journal.
func rollbackMove(ctx context.Context, cfg config.Config, m *byoc.Manager, step moveStep, out io.Writer) error {
	j, ok, err := loadMoveJournal(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("instance %s has no unfinished move to roll back", cfg.Name)
	}
	if j.Phase == phaseStarted {
		return fmt.Errorf("instance %s's move to %s finished but for removing the source's copy: resume it to finish", cfg.Name, j.To)
	}
	src, dst, err := journalEnds(ctx, cfg, m, j)
	if err != nil {
		return err
	}
	srcS, from, err := src.open(ctx, m, step)
	if err != nil {
		return err
	}
	if srcS != nil {
		defer srcS.Close()
	}
	dstS, toEnd, err := dst.open(ctx, m, step)
	if err != nil {
		return err
	}
	if dstS != nil {
		defer dstS.Close()
	}
	carry, err := remotecompute.OpenCarry(carryOptions(cfg, j, from, toEnd, step))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	undoMove(ctx, cfg, m, j, carry, step, out)
	return nil
}

// journalEnds is the two ends a journal names.
func journalEnds(ctx context.Context, cfg config.Config, m *byoc.Manager, j moveJournal) (moveEnd, moveEnd, error) {
	src := endOf(cfg, j.From)
	dst := endOf(cfg, j.Dest)
	if (src.machine != "" || dst.machine != "") && m == nil {
		return moveEnd{}, moveEnd{}, errors.New("no provisioner to read the move's VMs from")
	}
	return src, dst, nil
}

func carryOptions(cfg config.Config, j moveJournal, from, to remotecompute.DockerEndpoint, step moveStep) remotecompute.CarryOptions {
	if from.Host == "" {
		from.Helper = cfg.Cluster.NodeImage
	}
	if to.Host == "" {
		to.Helper = cfg.Cluster.NodeImage
	}
	return remotecompute.CarryOptions{Instance: cfg.Name, Clusters: instanceClusters(context.Background(), cfg, from),
		From: from, To: to, Progress: step, Journal: moveCarryPath(cfg)}
}

// undoMove is a rollback: the up at the destination stops, the source's
// record comes back, what the carry made at the destination goes, and the
// instance starts where it ran.
func undoMove(ctx context.Context, cfg config.Config, m *byoc.Manager, j moveJournal, carry *remotecompute.Carry, step moveStep, out io.Writer) {
	ctx = context.WithoutCancel(ctx)
	if info, ok := running(cfg); ok && info.PID > 0 {
		_ = stopRunning(cfg, out)
	}
	if j.From != nil {
		_ = remotecompute.SaveRecord(cfg.InstanceDir(), *j.From)
	} else {
		_ = remotecompute.RemoveRecord(cfg.InstanceDir())
	}
	if m != nil && j.DestMachine != "" {
		_ = m.SetUsedBy(j.DestMachine, "")
	}
	if m != nil && j.FromMachine != "" {
		_ = m.SetUsedBy(j.FromMachine, cfg.Name)
	}
	if carry != nil {
		carry.Rollback(ctx)
	}
	_ = os.Remove(moveCarryPath(cfg))
	_ = os.Remove(moveJournalPath(cfg))
	if j.WasUp {
		step("rollback", "Starting "+cfg.Name+" on "+endOf(cfg, j.From).String()+" again")
		if err := runDetached(j.UpArgs, 30*time.Minute, j.Offline, out, out); err != nil {
			step("rollback", "Could not start "+cfg.Name+" there again: "+err.Error())
		}
	}
}

// runMove runs a move's steps from the journal's phase on.
func runMove(ctx context.Context, cfg config.Config, m *byoc.Manager, j moveJournal, src, dst moveEnd, resumed bool, step moveStep, out io.Writer) error {
	interrupted := func(err error) error {
		step("interrupted", "The move was interrupted; resume it, or roll it back: "+err.Error())
		return fmt.Errorf("the move of %s was interrupted (%w); resume it with `cloudburrow cloud move %s --resume`, or undo it with `--rollback`", cfg.Name, err, cfg.Name)
	}
	var carry *remotecompute.Carry
	fail := func(cause error) error {
		if remotecompute.Interrupted(ctx, cause) {
			return interrupted(cause)
		}
		step("rollback", "The move failed: "+cause.Error())
		undoMove(ctx, cfg, m, j, carry, step, out)
		return cause
	}

	// 2. Both ends, and the carry.
	srcS, from, err := src.open(ctx, m, step)
	if err != nil {
		return fail(err)
	}
	if srcS != nil {
		defer srcS.Close()
	}
	dstS, toEnd, err := dst.open(ctx, m, step)
	if err != nil {
		return fail(err)
	}
	if dstS != nil {
		defer dstS.Close()
	}
	if !dst.local {
		r := dst.rec
		j.Dest = &r
		_ = saveMoveJournal(cfg, j)
	}
	co := carryOptions(cfg, j, from, toEnd, step)
	switch {
	case j.Phase == phaseCarrying && resumed:
		if _, statErr := os.Stat(co.Journal); statErr == nil {
			carry, err = remotecompute.ResumeCarry(ctx, co)
		} else {
			carry, err = remotecompute.StartCarry(ctx, co)
		}
	case j.Phase == phaseCarrying:
		carry, err = remotecompute.StartCarry(ctx, co)
	default:
		carry, err = remotecompute.OpenCarry(co)
	}
	if err != nil {
		if remotecompute.Interrupted(ctx, err) {
			return interrupted(err)
		}
		// StartCarry and ResumeCarry rolled the carry back themselves.
		step("rollback", "The move failed: "+err.Error())
		undoMove(ctx, cfg, m, j, nil, step, out)
		return fmt.Errorf("carry %s's state to %s: %w", cfg.Name, dst, err)
	}
	if j.Phase == phaseCarrying {
		j.Phase = phaseCarried
		if err := saveMoveJournal(cfg, j); err != nil {
			return fail(err)
		}
	}

	// 3. The destination is the instance's compute; up runs there.
	if j.Phase == phaseCarried {
		if dst.local {
			if err := remotecompute.RemoveRecord(cfg.InstanceDir()); err != nil {
				return fail(err)
			}
		} else if err := remotecompute.SaveRecord(cfg.InstanceDir(), *j.Dest); err != nil {
			return fail(err)
		}
		if dst.machine != "" && dst.machine != cfg.Name {
			if err := m.SetUsedBy(dst.machine, cfg.Name); err != nil {
				return fail(err)
			}
			// The VM is this instance's now, not that of the instance of
			// its own name that never ran.
			if neverStarted(cfg, dst.machine) {
				c := cfg
				c.Name = dst.machine
				_ = remotecompute.RemoveRecord(c.InstanceDir())
			}
		}
		j.Phase = phaseRecorded
		if err := saveMoveJournal(cfg, j); err != nil {
			return fail(err)
		}
	}
	if j.Phase == phaseRecorded {
		if dst.local {
			// The instance's connection to the VM forwarded the ports its
			// containers publish to this machine's: each must be free here
			// before they start, or Docker refuses them (address already in
			// use).
			if err := freeLocalPorts(ctx, cfg, src, carry.PublishedPorts(), step); err != nil {
				return fail(err)
			}
		}
		step("start", "Starting "+cfg.Name+" on "+dst.String())
		if err := runDetached(j.UpArgs, 30*time.Minute, j.Offline, out, out); err != nil {
			return fail(fmt.Errorf("start %s on %s: %w", cfg.Name, dst, err))
		}
		// 4. Checked: up is ready (runDetached waited for it), and the
		// cluster's nodes run there.
		step("verify", "Checking that "+cfg.Name+"'s cluster runs on "+dst.String())
		for _, name := range carry.Containers() {
			if !strings.HasPrefix(name, cfg.ClusterName()+"-") {
				continue
			}
			var st string
			var err error
			if dstS != nil {
				st, err = dstS.Run(ctx, "docker inspect --format '{{.State.Running}}' "+shellWord(name))
			} else {
				var b []byte
				b, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name).Output()
				st = string(b)
			}
			if err != nil || strings.TrimSpace(st) != "true" {
				return fail(fmt.Errorf("node %s is not running on %s after the move (%v %s)", name, dst, err, strings.TrimSpace(st)))
			}
		}
		j.Phase = phaseStarted
		if err := saveMoveJournal(cfg, j); err != nil {
			return fail(err)
		}
	}

	// 5. Done: the source's copy goes.
	step("cleanup", "Removing "+cfg.Name+"'s containers and volumes from "+src.String())
	if err := carry.RemoveSource(context.WithoutCancel(ctx)); err != nil {
		step("cleanup", "Some of the source's copy could not be removed: "+err.Error())
	}
	if m != nil && src.machine != "" && src.machine != cfg.Name {
		_ = m.SetUsedBy(src.machine, "")
	}
	_ = os.Remove(moveCarryPath(cfg))
	_ = os.Remove(moveJournalPath(cfg))
	step("done", fmt.Sprintf("%s runs on %s; %s carried", cfg.Name, dst, humanSize(carry.Bytes())))
	return nil
}

// movePortWait bounds the wait for this machine's ports to come free in a
// move back to it.
var movePortWait = 2 * time.Minute

// freeLocalPorts closes what is left of the instance's connections to the
// VM it leaves (an SSH master whose process is gone forwards the VM's
// published ports here still), then waits, bounded, until each of ports is
// free on this machine. Its error names each port still taken and what
// holds it; the move then rolls back to the VM, its data kept.
func freeLocalPorts(ctx context.Context, cfg config.Config, src moveEnd, ports []int, step moveStep) error {
	step("ports", "Closing the connection to "+src.String()+" and waiting for its forwarded ports to be free on this machine")
	if ended, err := remotecompute.CloseOrphanedMasters(ctx, cfg.InstanceDir()); err != nil {
		step("ports", "A connection to "+src.String()+" could not be closed: "+err.Error())
	} else if len(ended) > 0 {
		step("ports", fmt.Sprintf("Closed %d leftover connection(s) to %s", len(ended), src))
	}
	if len(ports) == 0 {
		return nil
	}
	if err := remotecompute.WaitPortsFree(ctx, ports, movePortWait, 250*time.Millisecond); err != nil {
		var held *remotecompute.PortsHeldError
		if errors.As(err, &held) {
			return fmt.Errorf("%w; stop what holds them and move again (the instance stays on %s)", err, src)
		}
		return err
	}
	step("ports", fmt.Sprintf("Ports %s are free on this machine", joinInts(ports)))
	return nil
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, ", ")
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

// instanceClusters is the instance's kind clusters on the Docker at from:
// its own, and the Kubernetes Engine API's extra clusters, named after it,
// that are not another instance's own cluster.
func instanceClusters(ctx context.Context, cfg config.Config, from remotecompute.DockerEndpoint) []string {
	out := []string{cfg.ClusterName()}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	names, err := kindClustersAt(ctx, from.Host)
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

// kindClustersAt is `kind get clusters` on the Docker at host ("" is this
// process's own).
func kindClustersAt(ctx context.Context, host string) ([]string, error) {
	if host == "" {
		return kindClusters(ctx)
	}
	cmd := exec.CommandContext(ctx, "kind", "get", "clusters")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+host, "DOCKER_CONTEXT=")
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

// move is `cloudburrow cloud move <instance> --to <vm>|ssh://…|local`, or
// `--resume` / `--rollback` for an unfinished one.
func (c cloudCmd) move(ctx context.Context) error {
	const usage = "usage: cloudburrow cloud move <instance> --to <vm>|ssh://user@host|local, or --resume, or --rollback"
	to := c.flags["to"]
	_, resume := c.flags["resume"]
	_, rollback := c.flags["rollback"]
	n := 0
	for _, b := range []bool{to != "", resume, rollback} {
		if b {
			n++
		}
	}
	if n != 1 {
		return errors.New(usage)
	}
	cfg := c.cfg
	if len(c.args) == 1 {
		cfg.Name = c.args[0]
	} else if len(c.args) > 1 {
		return errors.New(usage)
	}
	verb := "Moving " + cfg.Name + " to " + to + ": it stops, its cluster's state is copied, and it starts there."
	question := "Move it?"
	if resume || rollback {
		j, ok, err := loadMoveJournal(cfg)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("instance %s has no unfinished move", cfg.Name)
		}
		to = j.To
		verb, question = "Resuming the move of "+cfg.Name+" to "+to+".", "Resume it?"
		if rollback {
			verb, question = "Rolling back the move of "+cfg.Name+" to "+to+": what was made there goes, and it starts where it ran.", "Roll it back?"
		}
	}
	fmt.Fprintln(c.stdout, verb)
	ok, err := c.confirm(question)
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintln(c.stdout, "Nothing was changed.")
		}
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
	// ^C or a SIGTERM interrupts the move between two units of its work; it
	// stays resumable.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The steps go to the terminal and to the instance's move.json, which
	// the console follows (a console-started move names it).
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: cfg.Name, To: to, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	if path := os.Getenv(moveProgressEnv); path != "" {
		p.path = path
	}
	if resume || rollback || os.Getenv(moveProgressEnv) != "" {
		if b, err := os.ReadFile(p.path); err == nil {
			_ = json.Unmarshal(b, &p.st)
		}
		p.st.Done, p.st.Error, p.st.Interrupted, p.st.To = false, "", false, to
	}
	p.st.PID = os.Getpid()
	stepf := func(step, msg string) {
		fmt.Fprintf(c.stdout, "%-11s %s\n", step, msg)
		p.step(step, msg)
	}
	switch {
	case resume:
		err = resumeMove(ctx, cfg, c.m, stepf, c.stdout)
	case rollback:
		err = rollbackMove(ctx, cfg, c.m, stepf, c.stdout)
		if err == nil {
			p.st.RolledBack = true
		}
	default:
		err = moveInstance(ctx, cfg, c.m, to, stepf, c.stdout)
	}
	p.finish(err, cfg)
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

func (p *moveProgress) finish(err error, cfg config.Config) {
	p.st.Done = true
	if err != nil {
		p.st.Error = err.Error()
	}
	// A move that stopped short of done or undone resumes.
	if _, pending, _ := loadMoveJournal(cfg); pending {
		p.st.Interrupted = true
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

// consoleMover is the console's Move, Resume and Roll back, and Replace's
// guided VM change: `cloudburrow cloud move` (or `cloud scale`) in a
// process of its own, which outlives the `up` it stops.
type consoleMover struct{ cfg config.Config }

func (m consoleMover) StartMove(instance, to string) error {
	cfg := m.cfg
	cfg.Name = instance
	if err := m.idle(cfg); err != nil {
		return err
	}
	if _, pending, _ := loadMoveJournal(cfg); pending {
		return fmt.Errorf("%s has an unfinished move: resume it or roll it back first", instance)
	}
	if rec, had, err := remotecompute.LoadRecord(cfg.InstanceDir()); err != nil {
		return err
	} else if had && (rec.MachineName(instance) == to && rec.Provisioned || rec.Target.String() == to) {
		return fmt.Errorf("%s runs on %s already", instance, to)
	} else if !had && to == moveToLocal {
		return fmt.Errorf("%s runs on this machine already", instance)
	}
	return m.spawn(cfg, "move", to, "Starting the move of "+instance+" to "+to, "cloud", "move", instance, "--to", to)
}

func (m consoleMover) ResumeMove(instance string) error {
	cfg := m.cfg
	cfg.Name = instance
	if err := m.idle(cfg); err != nil {
		return err
	}
	j, pending, err := loadMoveJournal(cfg)
	if err != nil || !pending {
		return fmt.Errorf("%s has no unfinished move to resume", instance)
	}
	return m.spawn(cfg, "move", j.To, "Resuming the move of "+instance+" to "+j.To, "cloud", "move", instance, "--resume")
}

func (m consoleMover) RollbackMove(instance string) error {
	cfg := m.cfg
	cfg.Name = instance
	if err := m.idle(cfg); err != nil {
		return err
	}
	j, pending, err := loadMoveJournal(cfg)
	if err != nil || !pending {
		return fmt.Errorf("%s has no unfinished move to roll back", instance)
	}
	return m.spawn(cfg, "move", j.To, "Rolling back the move of "+instance+" to "+j.To, "cloud", "move", instance, "--rollback")
}

// StartReplace is Compute capacity's guided change of a VM to a size its
// family cannot resize to (`cloudburrow cloud scale <vm> --size … --yes`).
func (m consoleMover) StartReplace(instance, vm, size string) error {
	cfg := m.cfg
	cfg.Name = instance
	if err := m.idle(cfg); err != nil {
		return err
	}
	if _, pending, _ := loadMoveJournal(cfg); pending {
		return fmt.Errorf("%s has an unfinished move: resume it or roll it back first", instance)
	}
	return m.spawn(cfg, "replace", vm, "Replacing "+vm+" with a "+size+" VM", "cloud", "scale", vm, "--size", size, "--for", instance)
}

// StartDeleteWithData is Compute capacity's "Delete VM and this
// instance's data": `cloudburrow cloud delete --name <vm>
// --with-instance-data --yes` in a process of its own, since it stops the
// instance's up (which may serve this console). Its steps go to the
// instance's move.json, as a move's do.
func (m consoleMover) StartDeleteWithData(instance, vm string) error {
	cfg := m.cfg
	cfg.Name = instance
	if err := m.idle(cfg); err != nil {
		return err
	}
	if _, pending, _ := loadMoveJournal(cfg); pending {
		return fmt.Errorf("%s has an unfinished move: resume it or roll it back first", instance)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: instance, Kind: "delete", To: vm, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	p.step("queued", "Deleting "+vm+" and "+instance+"'s data on it")
	logf, err := os.OpenFile(filepath.Join(cfg.InstanceDir(), "move.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	child := exec.Command(exe, "cloud", "delete", "--with-instance-data", "--yes", "--name", vm, "--state-dir", cfg.StateDir)
	child.Env = append(spawnEnv(), moveProgressEnv+"="+p.path)
	child.Stdout, child.Stderr = logf, logf
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		p.finish(err, cfg)
		return fmt.Errorf("start the delete: %w", err)
	}
	p.st.PID = child.Process.Pid
	p.write()
	go func() { _ = child.Wait() }()
	return nil
}

// idle refuses while a move or replace of the instance still runs.
func (m consoleMover) idle(cfg config.Config) error {
	if st, ok := m.MoveStatus(cfg.Name); ok && !st.Done && !st.Interrupted {
		return fmt.Errorf("%s is being moved to %s already", cfg.Name, st.To)
	}
	return nil
}

func (m consoleMover) spawn(cfg config.Config, kind, to, first string, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: cfg.Name, Kind: kind, To: to, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	p.step("queued", first)
	logf, err := os.OpenFile(filepath.Join(cfg.InstanceDir(), "move.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	child := exec.Command(exe, append(args, "--yes", "--name", cfg.Name, "--state-dir", cfg.StateDir)...)
	child.Env = append(spawnEnv(), moveProgressEnv+"="+p.path)
	child.Stdout, child.Stderr = logf, logf
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		p.finish(err, cfg)
		return fmt.Errorf("start the %s: %w", kind, err)
	}
	p.st.PID = child.Process.Pid
	p.write()
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
	// A move whose process is gone before it finished (killed, or the
	// machine slept and restarted) is interrupted: it resumes or rolls back.
	if !st.Done && st.PID > 0 && !pidAlive(st.PID) {
		st.Done, st.Interrupted = true, true
	}
	if _, pending, _ := loadMoveJournal(cfg); pending && st.Done {
		st.Interrupted = true
	}
	return st, true
}

// moveProgressEnv names the progress file a console-started move writes.
const moveProgressEnv = "CLOUDBURROW_MOVE_PROGRESS"

// spawnEnv is the environment for a move, a change of VM or a delete that
// the console starts: this process's, but with DOCKER_HOST as it was before
// this process connected to its compute. Connected to a VM, this process's
// DOCKER_HOST is its own session's socket, which goes when the move stops
// this up: inherited, "this machine's Docker" would be that dead socket.
func spawnEnv() []string {
	env := os.Environ()
	c := currentCompute()
	if c == nil {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") {
			out = append(out, kv)
		}
	}
	if c.hadPrev {
		out = append(out, "DOCKER_HOST="+c.prev)
	}
	return out
}
