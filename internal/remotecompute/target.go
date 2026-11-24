// Package remotecompute runs an instance's compute (its Docker: the kind
// cluster and every container CloudBurrow starts) on a remote Linux host
// over one supervised SSH connection, while the CLI process, the console,
// the metadata server and every API endpoint stay on this machine (#2209,
// phase 1; docs/remote-compute.md).
//
// The connection is the system ssh, so the user's ssh config (ProxyJump,
// Port, HostName aliases) and agent apply. Through it:
//
//   - the host's Docker socket is forwarded to a local Unix socket, which
//     DOCKER_HOST names for every docker and kind command this process runs;
//   - each port a container publishes on the host's loopback (the kind API
//     server, the ingress gateway, a notebook runtime's Jupyter ...) is
//     forwarded back to the same port on this machine's loopback;
//   - each listener a workload reaches on "the host" (the API ports, the
//     metadata server, the relays) is carried by a reverse forward bound on
//     the host's Docker network gateway, which only containers can reach.
//
// No port is opened on the host beyond SSH. The connection reconnects with
// backoff and replays every forward it carried.
//
// The package is the Go API the console and the provisioners (phase 2)
// call: Connect, Status, Reconnect and Close, with structured progress
// Events; `cloudburrow up --compute ssh://user@host[:port]` is one caller.
package remotecompute

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Target is a remote compute host, ssh://[user@]host[:port].
type Target struct {
	User string `json:"user,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// ErrBadTarget is a --compute value that is not an ssh:// URL.
var ErrBadTarget = errors.New("compute target must be ssh://[user@]host[:port]")

// ParseTarget parses ssh://[user@]host[:port]. Only ssh is a scheme here;
// the provisioners' aws and azure targets are phase 2 (docs/gaps.md).
func ParseTarget(s string) (Target, error) {
	if s == "" {
		return Target{}, fmt.Errorf("%w: empty", ErrBadTarget)
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "ssh" || u.Host == "" {
		return Target{}, fmt.Errorf("%w: %q", ErrBadTarget, s)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return Target{}, fmt.Errorf("%w: %q has a path, query or fragment", ErrBadTarget, s)
	}
	if _, set := u.User.Password(); set {
		return Target{}, fmt.Errorf("%w: %q carries a password; CloudBurrow uses its own key, never a password", ErrBadTarget, s)
	}
	t := Target{User: u.User.Username(), Host: u.Hostname()}
	if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host, " \t'\"`$;&|<>") {
		return Target{}, fmt.Errorf("%w: host %q", ErrBadTarget, t.Host)
	}
	if strings.HasPrefix(t.User, "-") || strings.ContainsAny(t.User, " \t'\"`$;&|<>@") {
		return Target{}, fmt.Errorf("%w: user %q", ErrBadTarget, t.User)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Target{}, fmt.Errorf("%w: port %q", ErrBadTarget, p)
		}
		t.Port = n
	}
	return t, nil
}

// String is the target as ssh://[user@]host[:port].
func (t Target) String() string {
	h := t.Host
	if t.Port != 0 {
		h = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	} else if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if t.User != "" {
		return "ssh://" + t.User + "@" + h
	}
	return "ssh://" + h
}

// destination is ssh's [user@]host argument.
func (t Target) destination() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// keyName is the per-remote directory name under the instance's state:
// one key per host, so revoking one remote's key leaves the others.
func (t Target) keyName() string {
	r := strings.NewReplacer(":", "_", "/", "_", "[", "", "]", "", "@", "_at_")
	n := r.Replace(t.Host)
	if t.User != "" {
		n = t.User + "_at_" + n
	}
	if t.Port != 0 {
		n += "_" + strconv.Itoa(t.Port)
	}
	return n
}
