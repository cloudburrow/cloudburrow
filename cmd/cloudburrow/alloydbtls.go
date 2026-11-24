package main

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// The AlloyDB Admin API over TLS (#2191). Google serves it only over TLS,
// and the Java and Python connectors open only a TLS gRPC channel to it
// (they have no plaintext option), so beside the plaintext port the
// instance serves the same API on a TLS port, its certificate signed by
// the AlloyDB certificate authority, whose certificate `env` names in
// CLOUDBURROW_ALLOYDB_CA_FILE: a client trusts it as its roots (gRPC's
// GRPC_DEFAULT_SSL_ROOTS_FILE_PATH, a JVM trust store). The TLS listener
// terminates TLS, offering h2 (gRPC) and http/1.1 (REST), and relays the
// stream to the plaintext port, which serves both.

type alloyDBTLSState struct {
	Address string `json:"address"`
}

func alloyDBTLSStatePath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "alloydb-api-tls.json")
}

// alloyDBCAFile is where the CA certificate is written for clients.
func alloyDBCAFile(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "alloydb-ca.pem")
}

func recordedAlloyDBTLS(cfg config.Config) string {
	b, err := os.ReadFile(alloyDBTLSStatePath(cfg))
	if err != nil {
		return ""
	}
	var s alloyDBTLSState
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Address
}

type alloyDBTLSFront struct {
	ln       net.Listener
	upstream string
	wg       sync.WaitGroup
}

// serveAlloyDBTLS listens on host, at a port the system picks, and relays
// each TLS connection's stream to upstream, the plaintext API.
func serveAlloyDBTLS(cfg config.Config, api *alloydb.Server, host, upstream string) (*alloyDBTLSFront, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	tcfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
				if h, _, err := net.SplitHostPort(hello.Conn.LocalAddr().String()); err == nil {
					name = h
				}
			}
			return api.ServerCertificate(name)
		},
	}
	f := &alloyDBTLSFront{ln: tls.NewListener(ln, tcfg), upstream: upstream}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err == nil {
		_ = os.WriteFile(alloyDBCAFile(cfg), []byte(api.CACertPEM()), 0o644)
		b, _ := json.MarshalIndent(alloyDBTLSState{Address: ln.Addr().String()}, "", "  ")
		_ = os.WriteFile(alloyDBTLSStatePath(cfg), append(b, '\n'), 0o600)
	}
	f.wg.Add(1)
	go f.accept()
	return f, nil
}

func (f *alloyDBTLSFront) Addr() string {
	if f == nil {
		return ""
	}
	return f.ln.Addr().String()
}

func (f *alloyDBTLSFront) accept() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			t := c.(*tls.Conn)
			_ = t.SetDeadline(time.Now().Add(30 * time.Second))
			if err := t.Handshake(); err != nil {
				return
			}
			_ = t.SetDeadline(time.Time{})
			u, err := net.DialTimeout("tcp", f.upstream, 10*time.Second)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(u, t)
				if tc, ok := u.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() { _, _ = io.Copy(t, u); _ = t.CloseWrite(); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func (f *alloyDBTLSFront) Close() {
	if f == nil {
		return
	}
	_ = f.ln.Close()
	f.wg.Wait()
}
