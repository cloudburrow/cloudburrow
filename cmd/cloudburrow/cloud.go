package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// `cloudburrow cloud` (#2209 phases 2 and 3): what Settings > Compute and
// Compute capacity do in the console, at a terminal. The instance is --name's
// (the default instance otherwise); the state is <state dir>/byoc/byoc.json,
// shared with the console, and the credentials are keychain items.

const cloudUsage = `Usage: cloudburrow cloud <command> [flags]

Run this instance's compute on a VM in your own AWS account or Azure
subscription. The APIs and the console stay on this machine.

Commands:
  connect aws --start-url URL --sso-region REGION [--account ID/ROLE]
              Sign in through AWS IAM Identity Center (a code to confirm on
              AWS's page, which opens in your browser)
  connect aws --access-key
              Paste an access key instead, for an account without IAM
              Identity Center (read from the terminal, never echoed)
  connect azure [--tenant T] [--subscription ID]
              Sign in on Microsoft's page, then pick the subscription
  disconnect <connection> [--force]
              Remove a connection's credentials from the keychain
  create [--connection ID] [--region R] [--size small|medium|large|gpu]
         [--auto-stop 60m|0] [--connect ssm|ssh] [--yes]
              Show the hourly price, then create the instance's VM. On AWS
              CloudBurrow connects through Session Manager (nothing inbound
              open); --connect ssh opens SSH to your address only instead,
              as on Azure, and the firewall follows your address
  set [<instance>] --auto-stop 30m|2h|0
              Change the VM's auto-stop after create (0 is never); a running
              VM's setting changes at once, a stopped one's at its next start
  allow-my-ip [<instance>]
              Move the firewall's SSH rule to your current public address now
              (CloudBurrow also does so before each connection)
  status [--format json]
              Connections, and the instance's VM read from its cloud
  scale [<vm>] --size small|medium|large|gpu [--yes]
              Resize the VM (stopping and starting it where the cloud must),
              with the price difference shown first. To or from GPU, which
              no cloud resizes in place, a new VM of that size is made, the
              instance moves onto it with its state, and the old VM is
              deleted, as one action
  start, stop Start or stop the VM; its disk and the cluster's state stay
  delete [--yes]
              Delete everything tagged cloudburrow-instance=<name>, and verify
  resources   List everything tagged cloudburrow-instance=<name>
  move <instance> --to <vm>|ssh://user@host|local [--yes]
              Move an instance, with its cluster's state, from where it runs
              (this machine, a VM, or a host of your own) onto a VM made with
              create (in the same or another cloud), a host, or back to this
              machine (local): it stops, its state is copied, and it starts
              and is checked there; a failure rolls back
  move <instance> --resume | --rollback [--yes]
              Continue a move that was interrupted from where it stopped, or
              undo it

Every command takes --name, --state-dir and --config as up does.
`

// newCloudManager is the manager the commands use; tests replace it.
var newCloudManager = func(cfg config.Config) (*byoc.Manager, error) {
	dir := filepath.Join(cfg.StateDir, "byoc")
	ns := cfg.StateDir
	if abs, err := filepath.Abs(ns); err == nil {
		ns = abs
	}
	o := byoc.Options{
		Dir:      dir,
		Keychain: keychain.OS{Namespace: ns},
		Drivers:  []byoc.Driver{awsprov.NewDriver(awsprov.Options{ClientName: "CloudBurrow"}), azureprov.New(azureprov.Options{})},
		// Phase 1 (#2209): a running VM becomes the instance's compute.
		Engine: &remotecompute.Engine{StateDir: cfg.StateDir, SSHConfig: byocSSHConfig(), LocalState: func(instance string) bool {
			c := cfg
			c.Name = instance
			return localClusterExists(context.Background(), c)
		}},
	}
	if f, ok, err := loadByocFakes(); err != nil {
		return nil, err
	} else if ok {
		// The tests' and the walk-through's local fakes of the clouds.
		if o, err = fakeCloudOptions(f, o); err != nil {
			return nil, err
		}
	}
	return byoc.NewManager(o)
}

// cloudStdin is where interactive answers are read; tests replace it.
var cloudStdin io.Reader = os.Stdin

func runCloud(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || hasHelpFlag(args[:1]) || args[0] == "help" {
		fmt.Fprint(stdout, cloudUsage)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	sub, rest := args[0], args[1:]
	if hasHelpFlag(rest) {
		fmt.Fprint(stdout, cloudUsage)
		return nil
	}
	flags, rest, err := cloudFlags(sub, rest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	var positional []string
	var cfgArgs []string
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			cfgArgs = append(cfgArgs, rest[i:]...)
			break
		}
		positional = append(positional, rest[i])
	}
	cfg, err := config.Load(config.Options{Args: cfgArgs, Output: stderr})
	if err != nil {
		return err
	}
	m, err := newCloudManager(cfg)
	if err != nil {
		return err
	}
	// Session Manager tunnels this command opened end with it.
	defer m.CloseTunnels()
	ctx := context.Background()
	c := cloudCmd{m: m, cfg: cfg, flags: flags, args: positional, stdout: stdout, stderr: stderr, in: bufio.NewReader(cloudStdin)}
	switch sub {
	case "connect":
		return c.connect(ctx)
	case "disconnect":
		return c.disconnect()
	case "create":
		return c.create(ctx)
	case "status":
		return c.status(ctx)
	case "scale":
		return c.scale(ctx)
	case "start", "stop":
		return c.power(ctx, sub)
	case "delete":
		return c.delete(ctx)
	case "resources":
		return c.resources(ctx)
	case "move":
		return c.move(ctx)
	case "set":
		return c.set(ctx)
	case "allow-my-ip":
		return c.allowMyIP(ctx)
	}
	fmt.Fprintf(stderr, "unknown cloud command %q\n\n%s", sub, cloudUsage)
	return errUsage
}

// cloudFlags takes each command's own flags off args; the rest are config's.
func cloudFlags(sub string, args []string) (map[string]string, []string, error) {
	names := map[string][]string{
		"connect":    {"start-url", "sso-region", "account", "tenant", "subscription", "access-key!"},
		"disconnect": {"force!"},
		"create":     {"connection", "region", "size", "auto-stop", "disk-gib", "connect", "yes!"},
		"set":        {"auto-stop"},
		"status":     {"format"},
		"scale":      {"size", "for", "yes!"},
		"delete":     {"yes!"},
		"move":       {"to", "resume!", "rollback!", "yes!"},
	}[sub]
	out := map[string]string{}
	rest := args
	for _, n := range names {
		boolean := strings.HasSuffix(n, "!")
		n = strings.TrimSuffix(n, "!")
		v, found, r, err := splitFlag(rest, n, boolean)
		if err != nil {
			return nil, nil, err
		}
		rest = r
		if found {
			out[n] = v
		}
	}
	return out, rest, nil
}

type cloudCmd struct {
	m      *byoc.Manager
	cfg    config.Config
	flags  map[string]string
	args   []string
	stdout io.Writer
	stderr io.Writer
	in     *bufio.Reader
}

func (c cloudCmd) printEvent(e byoc.Event) {
	mark := map[byoc.EventStatus]string{byoc.StepRunning: "…", byoc.StepDone: "✓", byoc.StepFailed: "✗", byoc.StepRolledBack: "↺"}[e.Status]
	fmt.Fprintf(c.stdout, "%s %-12s %s\n", mark, e.Step, e.Message)
}

func (c cloudCmd) ask(prompt string) (string, error) {
	fmt.Fprint(c.stdout, prompt)
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no answer: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askSecret reads without echo from a terminal, else a line of input.
func (c cloudCmd) askSecret(prompt string) (string, error) {
	if f, ok := cloudStdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(c.stdout, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.stdout)
		return strings.TrimSpace(string(b)), err
	}
	return c.ask(prompt)
}

func (c cloudCmd) confirm(question string) (bool, error) {
	if _, ok := c.flags["yes"]; ok {
		return true, nil
	}
	a, err := c.ask(question + " [y/N] ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(a, "y") || strings.EqualFold(a, "yes"), nil
}

func (c cloudCmd) connect(ctx context.Context) error {
	if len(c.args) != 1 {
		return fmt.Errorf("name the cloud: cloudburrow cloud connect aws|azure")
	}
	cloud, err := byoc.ParseCloud(c.args[0])
	if err != nil {
		return err
	}
	if _, ok := c.flags["access-key"]; ok {
		if cloud != byoc.AWS {
			return fmt.Errorf("--access-key is AWS's fallback; Azure signs in on Microsoft's page")
		}
		id, err := c.ask("Access key ID: ")
		if err != nil {
			return err
		}
		secret, err := c.askSecret("Secret access key: ")
		if err != nil {
			return err
		}
		token, err := c.askSecret("Session token (empty if none): ")
		if err != nil {
			return err
		}
		conn, err := c.m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: id, SecretAccessKey: secret, SessionToken: token})
		if err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "Connected %s (%s); the key is in the keychain.\n", conn.ID, conn.Principal)
		return nil
	}
	opts := byoc.SignInOptions{StartURL: c.flags["start-url"], Region: c.flags["sso-region"], Tenant: c.flags["tenant"]}
	if cloud == byoc.AWS && (opts.StartURL == "" || opts.Region == "") {
		return fmt.Errorf("AWS signs in through IAM Identity Center: pass --start-url (your AWS access portal URL) and --sso-region, or use --access-key")
	}
	st, err := c.m.BeginSignIn(ctx, cloud, opts)
	if err != nil {
		return err
	}
	if st.Prompt.UserCode != "" {
		fmt.Fprintf(c.stdout, "Confirm the code %s on the page that opened in your browser:\n  %s\n", st.Prompt.UserCode, st.Prompt.URL)
	} else {
		fmt.Fprintln(c.stdout, st.Prompt.Message)
		if st.Prompt.URL != "" {
			fmt.Fprintln(c.stdout, "  "+st.Prompt.URL)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	st, err = c.m.WaitSignIn(wctx, st.ID)
	if err != nil {
		return err
	}
	if st.State == "failed" {
		return errors.New(st.Error)
	}
	if len(st.Choices) == 0 {
		return fmt.Errorf("the sign-in has no %s to connect", map[byoc.Cloud]string{byoc.AWS: "account or role", byoc.Azure: "subscription"}[cloud])
	}
	want := c.flags["account"]
	if cloud == byoc.Azure {
		want = c.flags["subscription"]
	}
	choice := ""
	switch {
	case want != "":
		choice = want
	case len(st.Choices) == 1:
		choice = st.Choices[0].ID
	default:
		for i, ch := range st.Choices {
			fmt.Fprintf(c.stdout, "  %d. %s\n", i+1, ch.Label)
		}
		a, err := c.ask("Connect which? ")
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 || n > len(st.Choices) {
			return fmt.Errorf("%q is not one of 1 to %d", a, len(st.Choices))
		}
		choice = st.Choices[n-1].ID
	}
	conn, err := c.m.ChooseAccount(ctx, st.ID, choice)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Connected %s; the credentials are in the keychain.\n", conn.ID)
	if r, err := c.m.CheckPermissions(ctx, conn.ID, ""); err == nil {
		fmt.Fprintln(c.stdout, r.Summary())
	}
	return nil
}

func (c cloudCmd) disconnect() error {
	if len(c.args) != 1 {
		return fmt.Errorf("name the connection: cloudburrow cloud disconnect <connection> (see cloudburrow cloud status)")
	}
	_, force := c.flags["force"]
	if err := c.m.Disconnect(c.args[0], force); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Disconnected %s: its credentials are no longer in the keychain.\n", c.args[0])
	return nil
}

func (c cloudCmd) create(ctx context.Context) error {
	conn, err := c.m.ResolveConnection("", c.flags["connection"])
	if err != nil {
		return err
	}
	region := c.flags["region"]
	if region == "" {
		rc, err := c.m.Regions(ctx, conn.ID)
		if err != nil {
			return err
		}
		region = rc.Default
	}
	preset := byoc.Small
	if s := c.flags["size"]; s != "" {
		if preset, err = byoc.ParsePreset(s); err != nil {
			return err
		}
	}
	autoStop := 60
	if v, ok := c.flags["auto-stop"]; ok {
		if autoStop, err = parseAutoStop(v); err != nil {
			return err
		}
	}
	disk := 0
	if v := c.flags["disk-gib"]; v != "" {
		if disk, err = strconv.Atoi(v); err != nil || disk < 30 || disk > 4096 {
			return fmt.Errorf("--disk-gib is 30 to 4096")
		}
	}
	sizes, err := c.m.Sizes(ctx, conn.ID, region)
	if err != nil {
		return err
	}
	var size byoc.Size
	for _, s := range sizes {
		if s.Preset == preset {
			size = s
		}
	}
	if size.Name == "" {
		return fmt.Errorf("no %s size in %s", preset, region)
	}
	fmt.Fprintf(c.stdout, "%s in %s %s, account %s\n", c.cfg.Name, conn.Cloud.Title(), region, conn.AccountID)
	fmt.Fprintf(c.stdout, "  %s: %s, %s\n", preset.Title(), size.Name, describeSize(size))
	fmt.Fprintf(c.stdout, "  Price: %s\n", priceLine(size))
	if size.Note != "" {
		fmt.Fprintf(c.stdout, "  Note: %s\n", size.Note)
	}
	if autoStop > 0 {
		fmt.Fprintf(c.stdout, "  Auto-stop: after %d idle minutes\n", autoStop)
	} else {
		fmt.Fprintln(c.stdout, "  Auto-stop: never (billed until you stop it)")
	}
	mode, err := byoc.ParseConnectMode(c.flags["connect"])
	if err != nil {
		return err
	}
	switch {
	case mode == byoc.ConnectSSH || (mode == "" && conn.Cloud != byoc.AWS):
		fmt.Fprintln(c.stdout, "  Connect: SSH, the firewall open to your public address only (it follows your address)")
	default:
		fmt.Fprintln(c.stdout, "  Connect: AWS Systems Manager Session Manager, nothing inbound open")
	}
	ok, err := c.confirm("Create it?")
	if err != nil || !ok {
		if err == nil {
			fmt.Fprintln(c.stdout, "Nothing was created.")
		}
		return err
	}
	op, err := c.m.Create(ctx, byoc.CreateRequest{Instance: c.cfg.Name, Connection: conn.ID, Region: region, Preset: preset,
		AutoStopMinutes: autoStop, DiskGiB: disk, ConnectMode: mode}, true, c.printEvent)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s is %s at %s.\n", c.cfg.Name, op.Machine.State, op.Machine.PublicIP)
	return nil
}

func parseAutoStop(v string) (int, error) {
	if v == "0" || v == "never" || v == "off" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > byoc.MaxAutoStopMinutes*time.Minute {
		return 0, fmt.Errorf("--auto-stop is a duration such as 30m or 2h (at most a week), or 0 for never")
	}
	return int(d / time.Minute), nil
}

func describeSize(s byoc.Size) string {
	out := fmt.Sprintf("%d vCPU, %g GB", s.VCPUs, s.MemoryGiB)
	if s.GPUs > 0 {
		out += fmt.Sprintf(", %d × %s", s.GPUs, s.GPU)
	}
	return out
}

func priceLine(s byoc.Size) string {
	if s.HourlyUSD == 0 {
		return "unavailable (" + s.PriceError + ")"
	}
	out := fmt.Sprintf("$%.3f/hour while running (about $%.2f a month)", s.HourlyUSD, s.HourlyUSD*730)
	if s.DiskHourlyUSD > 0 {
		out += fmt.Sprintf(", plus the disk at $%.4f/hour while it exists", s.DiskHourlyUSD)
	}
	return out
}

func (c cloudCmd) status(ctx context.Context) error {
	conns, err := c.m.Connections()
	if err != nil {
		return err
	}
	var mc *byoc.Machine
	var refreshErr error
	if got, err := c.m.Refresh(ctx, c.cfg.Name); err == nil {
		mc = &got
	} else if !errors.Is(err, byoc.ErrNotFound) {
		refreshErr = err
		if rec, rerr := c.m.Machine(c.cfg.Name); rerr == nil {
			mc = &rec
		}
	}
	if c.flags["format"] == "json" {
		out := map[string]any{"connections": conns, "instance": c.cfg.Name, "machine": mc}
		if refreshErr != nil {
			out["refresh_error"] = refreshErr.Error()
		}
		enc := json.NewEncoder(c.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CONNECTION\tCLOUD\tACCOUNT\tSIGNED IN AS")
	for _, cn := range conns {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cn.ID, cn.Cloud.Title(), cn.AccountName, cn.Principal)
	}
	tw.Flush()
	if len(conns) == 0 {
		fmt.Fprintln(c.stdout, "No cloud is connected: cloudburrow cloud connect aws|azure.")
	}
	fmt.Fprintln(c.stdout)
	if mc == nil {
		fmt.Fprintf(c.stdout, "%s has no VM: cloudburrow cloud create.\n", c.cfg.Name)
		return nil
	}
	fmt.Fprintf(c.stdout, "%s: %s, %s %s, %s, %s", mc.Instance, mc.State, mc.Cloud.Title(), mc.Region, mc.Size, mc.PublicIP)
	if mc.HourlyUSD > 0 {
		fmt.Fprintf(c.stdout, ", $%.3f/hour while running", mc.HourlyUSD)
	}
	fmt.Fprintln(c.stdout)
	if refreshErr != nil {
		fmt.Fprintf(c.stdout, "(as last recorded: %v)\n", refreshErr)
	}
	if mc.ConnectMode == byoc.ConnectSSM {
		fmt.Fprintln(c.stdout, "Connect: Session Manager; nothing inbound is open")
	} else {
		fmt.Fprintf(c.stdout, "Connect: SSH from %s only\n", orUnknown(mc.SourceCIDR))
	}
	fmt.Fprintf(c.stdout, "Auto-stop: %s\n", byoc.AutoStopWords(mc.AutoStopMinutes))
	if st, err := c.m.Engine().Status(ctx, mc.Instance); err == nil && st.Message != "" {
		fmt.Fprintln(c.stdout, st.Message)
	}
	return nil
}

func (c cloudCmd) scale(ctx context.Context) error {
	v := c.flags["size"]
	if v == "" {
		return fmt.Errorf("--size small|medium|large|gpu")
	}
	preset, err := byoc.ParsePreset(v)
	if err != nil {
		return err
	}
	vm := c.cfg.Name
	if len(c.args) == 1 {
		vm = c.args[0]
	} else if len(c.args) > 1 {
		return errors.New("usage: cloudburrow cloud scale [<vm>] --size small|medium|large|gpu [--yes]")
	}
	plan, err := c.m.PlanResize(ctx, vm, preset, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s: %s (%s) to %s (%s)\n", vm, plan.From.Name, priceLine(plan.From), plan.To.Name, describeSize(plan.To))
	fmt.Fprintf(c.stdout, "Price: %s, %s\n", priceLine(plan.To), deltaLine(plan.DeltaUSD))
	if plan.Note != "" {
		fmt.Fprintln(c.stdout, plan.Note+".")
	}
	if !plan.NewVM {
		ok, err := c.confirm("Resize it?")
		if err != nil || !ok {
			return err
		}
		_, err = c.m.Scale(ctx, vm, preset, "", true, c.printEvent)
		return err
	}
	ok, err := c.confirm("Make the new VM, move the instance onto it and delete " + vm + "?")
	if err != nil || !ok {
		return err
	}
	return c.replace(ctx, vm, plan)
}

// deltaLine is a price difference in words.
func deltaLine(d float64) string {
	switch {
	case d > 0.0005:
		return fmt.Sprintf("$%.3f/h more than now", d)
	case d < -0.0005:
		return fmt.Sprintf("$%.3f/h less than now", -d)
	}
	return "the same as now"
}

// vmUser is the instance whose compute vm is, if one is.
func vmUser(cfg config.Config, m *byoc.Manager, vm string) (string, bool) {
	mc, err := m.Machine(vm)
	if err != nil {
		return "", false
	}
	for _, inst := range []string{mc.UsedBy, mc.Instance} {
		if inst == "" {
			continue
		}
		c := cfg
		c.Name = inst
		if rec, had, _ := remotecompute.LoadRecord(c.InstanceDir()); had && rec.Provisioned && rec.MachineName(inst) == vm {
			return inst, true
		}
		if j, pending, _ := loadMoveJournal(c); pending && (j.DestMachine == vm || j.FromMachine == vm) {
			return inst, true
		}
	}
	return "", false
}

// moveInstanceFn is moveInstance; tests replace it.
var moveInstanceFn = moveInstance

// replaceVMs is the guided change across size families: a new VM of the
// plan's size, the instance (if one runs on vm) moved onto it with its
// state, and vm deleted. A failure before the old VM's delete leaves the
// instance where it was and the new VM deleted.
var replaceVMs = func(ctx context.Context, cfg config.Config, m *byoc.Manager, vm string, plan byoc.ResizePlan, step moveStep, event byoc.Progress, out io.Writer) error {
	instance, used := vmUser(cfg, m, vm)
	if c := cfg; used {
		c.Name = instance
		if err := pendingMove(c); err != nil {
			return err
		}
	}
	step("create", fmt.Sprintf("Making %s, a %s VM (%s)", plan.NewName, plan.To.Name, priceLine(plan.To)))
	if _, err := m.CreateLike(ctx, vm, plan.NewName, plan.To.Name, true, event); err != nil {
		return fmt.Errorf("make %s: %w (nothing else was changed)", plan.NewName, err)
	}
	dropNew := func(cause error) error {
		step("rollback", "Deleting "+plan.NewName+": "+cause.Error())
		if _, err := m.Delete(context.WithoutCancel(ctx), plan.NewName, true, event); err != nil {
			step("rollback", "Could not delete "+plan.NewName+": "+err.Error())
		}
		return cause
	}
	if used {
		c := cfg
		c.Name = instance
		step("move", "Moving "+instance+" from "+vm+" to "+plan.NewName)
		if err := moveInstanceFn(ctx, c, m, plan.NewName, step, out); err != nil {
			if remotecompute.Interrupted(ctx, err) {
				return fmt.Errorf("the change of %s was interrupted during the move: resume the move with `cloudburrow cloud move %s --resume` (or roll it back), then delete %s: %w", vm, instance, vm, err)
			}
			return dropNew(fmt.Errorf("move %s onto %s: %w", instance, plan.NewName, err))
		}
	}
	step("delete", "Deleting "+vm+", which nothing uses now")
	if _, err := m.Delete(ctx, vm, true, event); err != nil {
		return fmt.Errorf("%s runs on %s; %s could not be deleted, delete it again with `cloudburrow cloud delete %s`: %w", orNothing(instance), plan.NewName, vm, vm, err)
	}
	step("done", fmt.Sprintf("%s is %s now (%s); %s is deleted", plan.NewName, plan.To.Name, priceLine(plan.To), vm))
	return nil
}

func orNothing(s string) string {
	if s == "" {
		return "Nothing"
	}
	return s
}

// replace runs replaceVMs with its progress at the terminal and in the
// instance's move.json, which the console follows.
func (c cloudCmd) replace(ctx context.Context, vm string, plan byoc.ResizePlan) error {
	cfg := c.cfg
	if inst := c.flags["for"]; inst != "" {
		cfg.Name = inst
	} else if inst, ok := vmUser(c.cfg, c.m, vm); ok {
		cfg.Name = inst
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
	p := &moveProgress{path: moveFile(cfg), st: console.MoveStatus{Instance: cfg.Name, Kind: "replace", To: plan.NewName, Started: time.Now().UTC(), Steps: []console.MoveStep{}}}
	if path := os.Getenv(moveProgressEnv); path != "" {
		p.path = path
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &p.st)
		}
		p.st.To = plan.NewName
	}
	p.st.PID = os.Getpid()
	err := replaceVMs(ctx, c.cfg, c.m, vm, plan, func(step, msg string) {
		fmt.Fprintf(c.stdout, "%-11s %s\n", step, msg)
		p.step(step, msg)
	}, c.printEvent, c.stdout)
	p.finish(err, cfg)
	return err
}

func (c cloudCmd) power(ctx context.Context, which string) error {
	var err error
	if which == "start" {
		_, err = c.m.Start(ctx, c.cfg.Name, true, c.printEvent)
	} else {
		_, err = c.m.Stop(ctx, c.cfg.Name, true, c.printEvent)
	}
	return err
}

func (c cloudCmd) delete(ctx context.Context) error {
	mc, err := c.m.Machine(c.cfg.Name)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Deletes every resource tagged %s=%s in %s %s, the VM's disk with it.\n", byoc.TagKey, mc.Instance, mc.Cloud.Title(), mc.Region)
	ok, err := c.confirm("Delete it?")
	if err != nil || !ok {
		return err
	}
	if _, err := c.m.Delete(ctx, c.cfg.Name, true, c.printEvent); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Deleted: nothing tagged %s=%s is left.\n", byoc.TagKey, mc.Instance)
	return nil
}

func (c cloudCmd) resources(ctx context.Context) error {
	rs, err := c.m.Resources(ctx, c.cfg.Name)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tID\tREGION\tSTATE")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Type, r.ID, r.Region, r.State)
	}
	return tw.Flush()
}

func orUnknown(s string) string {
	if s == "" {
		return "an unrecorded address"
	}
	return s
}

// instanceArg is the command's instance: its one positional argument, or
// --name's.
func (c cloudCmd) instanceArg() (string, error) {
	switch len(c.args) {
	case 0:
		return c.cfg.Name, nil
	case 1:
		return c.args[0], nil
	}
	return "", fmt.Errorf("one instance at most, got %v", c.args)
}

func (c cloudCmd) set(ctx context.Context) error {
	inst, err := c.instanceArg()
	if err != nil {
		return err
	}
	v, ok := c.flags["auto-stop"]
	if !ok {
		return fmt.Errorf("set what? --auto-stop 30m|2h|0 (the size is `cloudburrow cloud scale --size`)")
	}
	minutes, err := parseAutoStop(v)
	if err != nil {
		return err
	}
	op, err := c.m.SetAutoStop(ctx, inst, minutes, true, c.printEvent)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s: auto-stop %s.\n", inst, byoc.AutoStopWords(op.Machine.AutoStopMinutes))
	return nil
}

func (c cloudCmd) allowMyIP(ctx context.Context) error {
	inst, err := c.instanceArg()
	if err != nil {
		return err
	}
	op, err := c.m.AllowMyAddress(ctx, inst, true, c.printEvent)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s: SSH is open to %s only.\n", inst, op.Machine.SourceCIDR)
	return nil
}
