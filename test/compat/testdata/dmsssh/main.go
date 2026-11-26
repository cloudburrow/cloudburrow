// Command dmsssh is the SSH side of the Database Migration Service compat
// tests' tunnels (#2257), on golang.org/x/crypto/ssh:
//
//	dmsssh server -listen :22 -user U -password P
//
// is a bastion: an SSH server that signs in U with P and opens
// direct-tcpip channels (a forward-SSH tunnel's) and remote forwards
// (tcpip-forward, a reverse-SSH tunnel's, bound on every address as
// OpenSSH's GatewayPorts yes binds them);
//
//	dmsssh reverse -ssh HOST:22 -user U -password P -remote 0.0.0.0:PORT -target SRC:PORT
//
// is the source's side of a reverse-SSH tunnel, as `ssh -N -R
// 0.0.0.0:PORT:SRC:PORT U@HOST` runs it: the bastion's PORT forwards to
// the source.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: dmsssh server|reverse ...")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", ":22", "")
	user := fs.String("user", "", "")
	password := fs.String("password", "", "")
	sshAddr := fs.String("ssh", "", "")
	remote := fs.String("remote", "", "")
	target := fs.String("target", "", "")
	_ = fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "server":
		serve(*listen, *user, *password)
	case "reverse":
		for {
			err := reverse(*sshAddr, *user, *password, *remote, *target)
			log.Printf("reverse: %v; again", err)
			time.Sleep(time.Second)
		}
	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

func pipe(a, b io.ReadWriteCloser) {
	go func() { _, _ = io.Copy(a, b); _ = a.Close() }()
	_, _ = io.Copy(b, a)
	_ = b.Close()
}

func serve(listen, user, password string) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		log.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == user && string(p) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("dmsssh server on %s", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
			if err != nil {
				log.Printf("handshake: %v", err)
				return
			}
			go globalRequests(conn, reqs)
			for nc := range chans {
				if nc.ChannelType() != "direct-tcpip" {
					_ = nc.Reject(ssh.UnknownChannelType, "only direct-tcpip")
					continue
				}
				var p struct {
					Host     string
					Port     uint32
					OrigHost string
					OrigPort uint32
				}
				if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
					_ = nc.Reject(ssh.ConnectionFailed, err.Error())
					continue
				}
				out, err := net.DialTimeout("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))), 10*time.Second)
				if err != nil {
					_ = nc.Reject(ssh.ConnectionFailed, err.Error())
					continue
				}
				ch, rq, err := nc.Accept()
				if err != nil {
					_ = out.Close()
					continue
				}
				go ssh.DiscardRequests(rq)
				log.Printf("direct-tcpip to %s:%d", p.Host, p.Port)
				go pipe(ch, out)
			}
		}()
	}
}

// globalRequests serves tcpip-forward: a listener on the bastion whose
// connections are forwarded-tcpip channels to the client.
func globalRequests(conn *ssh.ServerConn, reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "tcpip-forward" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var p struct {
			Addr string
			Port uint32
		}
		if err := ssh.Unmarshal(r.Payload, &p); err != nil {
			_ = r.Reply(false, nil)
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(p.Port))))
		if err != nil {
			log.Printf("tcpip-forward %d: %v", p.Port, err)
			_ = r.Reply(false, nil)
			continue
		}
		_ = r.Reply(true, nil)
		log.Printf("tcpip-forward on %s", l.Addr())
		go func() {
			<-waitClosed(conn)
			_ = l.Close()
		}()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				ra := c.RemoteAddr().(*net.TCPAddr)
				payload := ssh.Marshal(struct {
					Addr     string
					Port     uint32
					OrigAddr string
					OrigPort uint32
				}{p.Addr, p.Port, ra.IP.String(), uint32(ra.Port)})
				ch, rq, err := conn.OpenChannel("forwarded-tcpip", payload)
				if err != nil {
					_ = c.Close()
					continue
				}
				go ssh.DiscardRequests(rq)
				go pipe(ch, c)
			}
		}()
	}
}

func waitClosed(conn *ssh.ServerConn) <-chan struct{} {
	done := make(chan struct{})
	go func() { _ = conn.Wait(); close(done) }()
	return done
}

func reverse(addr, user, password, remote, target string) error {
	cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}) //nolint:gosec // a test bastion
	if err != nil {
		return err
	}
	defer cl.Close()
	l, err := cl.Listen("tcp", remote)
	if err != nil {
		return err
	}
	log.Printf("reverse: %s on the bastion forwards to %s", remote, target)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			out, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				log.Printf("reverse: %v", err)
				_ = c.Close()
				return
			}
			pipe(c, out)
		}()
	}
}
