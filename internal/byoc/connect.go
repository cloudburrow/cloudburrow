package byoc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// How CloudBurrow reaches each VM (#2209): through AWS Systems Manager
// Session Manager on AWS, with nothing inbound open, or SSH to the VM's
// address with the firewall open to the user's own address only, which
// follows that address as it changes. And the settings a VM keeps after
// create: auto-stop (here) and its size (Scale).

// connectModeFor is the mode a create uses.
func connectModeFor(c Cloud, p Provider, asked string) (string, error) {
	_, tunnels := p.(Tunneler)
	switch asked {
	case "":
		if tunnels {
			return ConnectSSM, nil
		}
		return ConnectSSH, nil
	case ConnectSSH:
		return ConnectSSH, nil
	case ConnectSSM:
		if !tunnels {
			return "", fmt.Errorf("%s has no Session Manager: CloudBurrow reaches its VMs over SSH, the firewall open to your address only (--connect ssh)", c.Title())
		}
		return ConnectSSM, nil
	}
	return "", fmt.Errorf("unknown connect mode %q: want ssm or ssh", asked)
}

// ParseConnectMode reads a mode as the CLI and the console send it.
func ParseConnectMode(s string) (string, error) {
	switch s {
	case "", ConnectSSM, ConnectSSH:
		return s, nil
	case "session-manager":
		return ConnectSSM, nil
	}
	return "", fmt.Errorf("unknown connect mode %q: want ssm or ssh", s)
}

// currentCIDR is the user's public address as a single-address CIDR.
func (m *Manager) currentCIDR(ctx context.Context) (string, error) {
	ip, err := m.ipLookup(ctx)
	if err != nil {
		return "", fmt.Errorf("finding your public address: %w", err)
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", fmt.Errorf("finding your public address: %q is not an address", ip)
	}
	if parsed.To4() == nil {
		return ip + "/128", nil
	}
	return ip + "/32", nil
}

// RemoteTargetContext is what the remote engine needs to reach a machine:
// for SSM, a Session Manager tunnel on 127.0.0.1 (opened once, kept for
// the process); for SSH, the VM's address, the firewall first moved to the
// user's current address.
func (m *Manager) RemoteTargetContext(ctx context.Context, instance string, progress Progress) (RemoteTarget, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return RemoteTarget{}, err
	}
	key, err := m.keys.Get(keyName(instance))
	if err != nil {
		return RemoteTarget{}, err
	}
	t := RemoteTarget{Instance: instance, Host: mc.PublicIP, Port: mc.SSHPort, User: mc.SSHUser, PrivateKey: key, HostKey: mc.HostKey, Via: ConnectSSH}
	if mc.ConnectMode == ConnectSSM {
		tun, err := m.tunnel(ctx, mc, progress)
		if err != nil {
			return RemoteTarget{}, err
		}
		t.Host, t.Port, t.Via = "127.0.0.1", tun.Port(), ConnectSSM
		return t, nil
	}
	if _, _, err := m.FollowAddress(ctx, instance, progress); err != nil {
		// The address may not have changed; the connection says if it did.
		progress.Report("address", StepFailed, "Could not check this machine's public address: %v", err)
	}
	t.BeforeConnect = func(ctx context.Context) error {
		_, _, err := m.FollowAddress(ctx, instance, nil)
		return err
	}
	return t, nil
}

func (m *Manager) tunnel(ctx context.Context, mc Machine, progress Progress) (Tunnel, error) {
	m.mu.Lock()
	tun := m.tunnels[mc.Instance]
	m.mu.Unlock()
	if tun != nil {
		return tun, nil
	}
	p, err := m.Provider(ctx, mc.Connection)
	if err != nil {
		return nil, err
	}
	tp, ok := p.(Tunneler)
	if !ok {
		return nil, fmt.Errorf("%s connects through Session Manager, which %s does not offer", mc.Instance, mc.Cloud.Title())
	}
	port := mc.SSHPort
	if port == 0 {
		port = 22
	}
	progress.Report("ssm", StepRunning, "Waiting for the VM's SSM agent to register with Systems Manager")
	tun, err = tp.OpenTunnel(ctx, mc, port, progress)
	if err != nil {
		progress.Report("ssm", StepFailed, "%v", err)
		return nil, err
	}
	progress.Report("ssm", StepDone, "Session Manager carries SSH to the VM; nothing inbound is open")
	m.mu.Lock()
	if old := m.tunnels[mc.Instance]; old != nil {
		m.mu.Unlock()
		_ = tun.Close()
		return old, nil
	}
	m.tunnels[mc.Instance] = tun
	m.mu.Unlock()
	return tun, nil
}

func (m *Manager) closeTunnel(instance string) {
	m.mu.Lock()
	tun := m.tunnels[instance]
	delete(m.tunnels, instance)
	m.mu.Unlock()
	if tun != nil {
		_ = tun.Close()
	}
}

// CloseTunnels ends every Session Manager tunnel (the process exits).
func (m *Manager) CloseTunnels() {
	m.mu.Lock()
	names := make([]string, 0, len(m.tunnels))
	for n := range m.tunnels {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, n := range names {
		m.closeTunnel(n)
	}
}

// FollowAddress moves an SSH-mode machine's firewall to the user's current
// public address, if it changed; changed reports whether it did. An SSM
// machine has no inbound rule to move.
func (m *Manager) FollowAddress(ctx context.Context, instance string, progress Progress) (Machine, bool, error) {
	m.addrMu.Lock()
	defer m.addrMu.Unlock()
	mc, err := m.Machine(instance)
	if err != nil {
		return Machine{}, false, err
	}
	if mc.ConnectMode == ConnectSSM {
		return mc, false, nil
	}
	cidr, err := m.currentCIDR(ctx)
	if err != nil {
		return mc, false, err
	}
	if cidr == mc.SourceCIDR {
		return mc, false, nil
	}
	p, err := m.Provider(ctx, mc.Connection)
	if err != nil {
		return mc, false, err
	}
	old := mc.SourceCIDR
	progress.Report("address", StepRunning, "Your public address is now %s: moving the firewall's SSH rule from %s", cidr, orNone(old))
	got, err := p.UpdateSourceAddress(ctx, mc, cidr)
	if err != nil {
		progress.Report("address", StepFailed, "%v", err)
		return mc, false, err
	}
	got.SourceCIDR, got.SourceUpdatedAt = cidr, m.now()
	if err := m.putMachine(got); err != nil {
		return got, true, err
	}
	m.noteAddress(instance, old, cidr)
	progress.Report("address", StepDone, "SSH is open to %s only", cidr)
	return got, true, nil
}

func orNone(s string) string {
	if s == "" {
		return "no address"
	}
	return s
}

// AddressChange is one move of a machine's firewall.
type AddressChange struct {
	Time time.Time `json:"time"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

func (m *Manager) noteAddress(instance, from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addrLog == nil {
		m.addrLog = map[string][]AddressChange{}
	}
	l := append(m.addrLog[instance], AddressChange{Time: m.now(), From: from, To: to})
	if len(l) > 20 {
		l = l[len(l)-20:]
	}
	m.addrLog[instance] = l
}

// AddressChanges is this process's moves of the machine's firewall.
func (m *Manager) AddressChanges(instance string) []AddressChange {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AddressChange(nil), m.addrLog[instance]...)
}

// AllowMyAddress is FollowAddress as an operation, for the console's and
// the CLI's "allow my address".
func (m *Manager) AllowMyAddress(ctx context.Context, instance string, wait bool, also Progress) (Operation, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return Operation{}, err
	}
	if mc.ConnectMode == ConnectSSM {
		return Operation{}, fmt.Errorf("%s connects through Session Manager: its firewall allows nothing inbound, so there is no address to update", instance)
	}
	return m.run("address", instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		got, changed, err := m.FollowAddress(ctx, instance, progress)
		if err != nil {
			return nil, err
		}
		if !changed {
			progress.Report("address", StepDone, "SSH is already open to %s, your current address", got.SourceCIDR)
		}
		return &got, nil
	})
}

// WatchAddresses checks the user's public address every interval until ctx
// ends, moving each running SSH-mode machine's firewall when it changes
// (the console's process runs it).
func (m *Manager) WatchAddresses(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = 3 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ms, _ := m.Machines()
		for _, mc := range ms {
			if mc.ConnectMode == ConnectSSM || mc.State != Running {
				continue
			}
			if _, changed, err := m.FollowAddress(ctx, mc.Instance, nil); err != nil && logf != nil {
				logf("cloud: %s: the firewall could not follow your address: %v", mc.Instance, err)
			} else if changed && logf != nil {
				got, _ := m.Machine(mc.Instance)
				logf("cloud: %s: your public address changed; SSH is now open to %s only", mc.Instance, got.SourceCIDR)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AutoStopFile is the VM's auto-stop setting, minutes as one line (0
// never), which its idle check reads each minute and CloudBurrow rewrites
// over SSH; the login user owns it (cloud-init).
const AutoStopFile = "/var/lib/cloudburrow/auto-stop"

// autoStopCommand writes minutes to the VM's setting.
func autoStopCommand(minutes int) string {
	n := strconv.Itoa(minutes)
	return `f=` + AutoStopFile + `; if [ -w "$f" ]; then printf '%s\n' ` + n + ` > "$f"; ` +
		`else sudo -n sh -c 'mkdir -p /var/lib/cloudburrow && printf "%s\n" ` + n + ` > ` + AutoStopFile + `' && sudo -n chown "$(id -un)" "$f"; fi && cat "$f"`
}

// ErrNoRunner is an engine that cannot run a command on the VM.
var ErrNoRunner = errors.New("the remote-host engine cannot run commands on the VM")

func (m *Manager) pushAutoStop(ctx context.Context, t RemoteTarget, minutes int) error {
	r, ok := m.engine.(RemoteRunner)
	if !ok {
		return ErrNoRunner
	}
	out, err := r.Run(ctx, t, autoStopCommand(minutes))
	if err != nil {
		return err
	}
	if got, perr := strconv.Atoi(trimSpace(out)); perr != nil || got != minutes {
		return fmt.Errorf("the VM's auto-stop setting reads %q after writing %d", trimSpace(out), minutes)
	}
	return nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == '\n' || s[0] == ' ') {
		s = s[1:]
	}
	return s
}

// SetAutoStop changes a machine's auto-stop after create. A running VM's
// setting is rewritten over SSH at once; a stopped one's at its next start.
func (m *Manager) SetAutoStop(ctx context.Context, instance string, minutes int, wait bool, also Progress) (Operation, error) {
	if minutes < 0 || minutes > MaxAutoStopMinutes {
		return Operation{}, fmt.Errorf("auto-stop must be between 0 (never) and %d minutes", MaxAutoStopMinutes)
	}
	if _, err := m.Machine(instance); err != nil {
		return Operation{}, err
	}
	return m.run("settings", instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		mc, err := m.Machine(instance)
		if err != nil {
			return nil, err
		}
		was := mc.AutoStopMinutes
		mc.AutoStopMinutes = minutes
		if err := m.putMachine(mc); err != nil {
			return nil, err
		}
		if mc.State != Running {
			progress.Report("auto-stop", StepDone, "Auto-stop is now %s; the VM is %s, so it applies when the VM next starts", AutoStopWords(minutes), mc.State)
			return &mc, nil
		}
		progress.Report("auto-stop", StepRunning, "Writing the new auto-stop setting (%s) on the VM", AutoStopWords(minutes))
		t, err := m.RemoteTargetContext(ctx, instance, progress)
		if err == nil {
			err = m.pushAutoStop(ctx, t, minutes)
		}
		if err != nil {
			mc.AutoStopMinutes = was
			_ = m.putMachine(mc)
			progress.Report("auto-stop", StepFailed, "%v", err)
			return &mc, fmt.Errorf("the VM's auto-stop was left at %s: %w", AutoStopWords(was), err)
		}
		progress.Report("auto-stop", StepDone, "Auto-stop is now %s", AutoStopWords(minutes))
		return &mc, nil
	})
}

// AutoStopWords is minutes as the console says it.
func AutoStopWords(minutes int) string {
	switch {
	case minutes == 0:
		return "never"
	case minutes%60 == 0:
		h := minutes / 60
		if h == 1 {
			return "after 1 hour idle"
		}
		return fmt.Sprintf("after %d hours idle", h)
	}
	return fmt.Sprintf("after %d minutes idle", minutes)
}
