package dmstunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshServer is an SSH host that accepts user/password (or key) and opens
// direct-tcpip channels to what it can dial.
func sshServer(t *testing.T, password string, key ssh.PublicKey) string {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == "bastion" && password != "" && string(p) == password {
				return nil, nil
			}
			return nil, io.EOF
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if key != nil && string(k.Marshal()) == string(key.Marshal()) {
				return nil, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "direct-tcpip" {
						_ = nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var p struct {
						Host string
						Port uint32
						OH   string
						OP   uint32
					}
					_ = ssh.Unmarshal(nc.ExtraData(), &p)
					out, err := net.Dial("tcp", net.JoinHostPort(p.Host, itoa(p.Port)))
					if err != nil {
						_ = nc.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, rq, _ := nc.Accept()
					go ssh.DiscardRequests(rq)
					go func() { _, _ = io.Copy(ch, out); _ = ch.Close() }()
					go func() { _, _ = io.Copy(out, ch); _ = out.Close() }()
				}
			}()
		}
	}()
	return l.Addr().String()
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// echo is the "source": it answers each line upper-cased.
func echo(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = io.WriteString(c, strings.ToUpper(line))
				}
			}()
		}
	}()
	return l.Addr().String()
}

func through(t *testing.T, c Config) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, l, c, io.Discard) }()
	// Two connections, each answered, over the one SSH connection.
	for i := 0; i < 2; i++ {
		conn, err := net.DialTimeout("tcp", l.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(conn, "select 1\n"); err != nil {
			t.Fatal(err)
		}
		got, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || got != "SELECT 1\n" {
			t.Fatalf("through the tunnel: %q %v", got, err)
		}
		_ = conn.Close()
	}
}

func TestTunnelPassword(t *testing.T) {
	src := echo(t)
	through(t, Config{Target: src, SSH: sshServer(t, "s3cret", nil), User: "bastion", Password: "s3cret"})
}

func TestTunnelPrivateKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	src := echo(t)
	through(t, Config{Target: src, SSH: sshServer(t, "", sp), User: "bastion", PrivateKey: string(pem.EncodeToMemory(blk))})
}

func TestTunnelPlainProxy(t *testing.T) {
	through(t, Config{Target: echo(t)})
}

func TestTunnelWrongPassword(t *testing.T) {
	if _, err := Dial(Config{SSH: sshServer(t, "s3cret", nil), User: "bastion", Password: "nope"}); err == nil {
		t.Fatal("a wrong password signed in")
	}
	if _, err := AuthMethods(Config{}); err == nil {
		t.Fatal("no credentials accepted")
	}
}
