// Package dmstunnel is the tunnel a Database Migration Service source
// profile's forward SSH connectivity runs (#2257): `cloudburrow-storage
// dms-tunnel` in a pod of the instance's cluster listens on a port the
// destinations' servers reach by its Service, signs in to the profile's
// SSH host (forwardSshConnectivity's hostname, port, username and password
// or private key) and carries each connection to the source's host and
// port over the SSH connection, as a direct-tcpip channel: what
// Database Migration Service's own tunnel does for a source reachable
// only from the SSH host
// (https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.connectionProfiles#forwardsshtunnelconnectivity,
// read 2026-11-25). Without --ssh it is a plain TCP proxy.
//
// Google publishes no host key field, so the SSH host's key is not
// checked, as Database Migration Service's cannot be (UNVERIFIED: the
// reference names none; docs re-read 2026-11-25, still unanswered: the
// forwardSshTunnelConnectivity reference above).
package dmstunnel

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ErrUsage is a command line Run cannot parse.
var ErrUsage = errors.New("usage")

// Env names the credentials Run reads, so they are on no command line.
const (
	EnvPassword   = "DMS_SSH_PASSWORD"
	EnvPrivateKey = "DMS_SSH_PRIVATE_KEY"
)

// Config is one tunnel.
type Config struct {
	// Listen is the address the tunnel accepts connections on.
	Listen string
	// Target is the source's host:port, as the SSH host reaches it (or,
	// with no SSH, as the tunnel does).
	Target string
	// SSH is the SSH host's host:port; "" proxies Target directly.
	SSH        string
	User       string
	Password   string
	PrivateKey string
}

// Run is `cloudburrow-storage dms-tunnel --listen ADDR --target HOST:PORT
// [--ssh HOST:PORT --ssh-user USER]`, the credentials from the environment.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dms-tunnel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c Config
	fs.StringVar(&c.Listen, "listen", "0.0.0.0:5432", "address to accept connections on")
	fs.StringVar(&c.Target, "target", "", "the source's host:port")
	fs.StringVar(&c.SSH, "ssh", "", "the SSH host's host:port")
	fs.StringVar(&c.User, "ssh-user", "", "the SSH user")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if c.Target == "" || (c.SSH != "" && c.User == "") {
		fmt.Fprintln(stderr, "dms-tunnel: --target is required, and --ssh-user with --ssh")
		return ErrUsage
	}
	c.Password, c.PrivateKey = os.Getenv(EnvPassword), os.Getenv(EnvPrivateKey)
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "dms-tunnel: %s -> %s via %q\n", l.Addr(), c.Target, c.SSH)
	return Serve(ctx, l, c, stderr)
}

// Serve carries l's connections until ctx ends.
func Serve(ctx context.Context, l net.Listener, c Config, logw io.Writer) error {
	t := &tunnel{c: c, log: logw}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				t.close()
				return nil
			}
			return err
		}
		go t.carry(conn)
	}
}

type tunnel struct {
	c   Config
	log io.Writer

	mu     sync.Mutex
	client *ssh.Client
}

func (t *tunnel) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		_ = t.client.Close()
		t.client = nil
	}
}

// AuthMethods are the SSH sign-ins c offers: its private key, then its
// password.
func AuthMethods(c Config) ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod
	if c.PrivateKey != "" {
		k, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("the SSH private key: %w", err)
		}
		out = append(out, ssh.PublicKeys(k))
	}
	if c.Password != "" {
		out = append(out, ssh.Password(c.Password))
	}
	if len(out) == 0 {
		return nil, errors.New("neither an SSH password nor a private key")
	}
	return out, nil
}

// Dial signs in to c's SSH host.
func Dial(c Config) (*ssh.Client, error) {
	auth, err := AuthMethods(c)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{User: c.User, Auth: auth, Timeout: 15 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey()} //nolint:gosec // the API carries no host key to check (package doc)
	return ssh.Dial("tcp", c.SSH, cfg)
}

// sshClient is the SSH connection, made again when the last one failed.
func (t *tunnel) sshClient() (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		if _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return t.client, nil
		}
		_ = t.client.Close()
		t.client = nil
	}
	cl, err := Dial(t.c)
	if err != nil {
		return nil, err
	}
	t.client = cl
	return cl, nil
}

func (t *tunnel) carry(in net.Conn) {
	defer in.Close()
	var out net.Conn
	var err error
	if t.c.SSH == "" {
		out, err = net.DialTimeout("tcp", t.c.Target, 15*time.Second)
	} else {
		var cl *ssh.Client
		if cl, err = t.sshClient(); err == nil {
			out, err = cl.Dial("tcp", t.c.Target)
		}
	}
	if err != nil {
		fmt.Fprintf(t.log, "dms-tunnel: %s: %v\n", t.c.Target, err)
		return
	}
	defer out.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(out, in); closeWrite(out); done <- struct{}{} }()
	go func() { _, _ = io.Copy(in, out); closeWrite(in); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
