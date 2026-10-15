package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// acceptAndDo listens on loopback and runs serve on every connection.
func acceptAndDo(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// A tunnel that accepts and closes, as the restarted instance's did
// (#1262), is not a ready database, whichever protocol.
func TestDatabaseHandshakeRefusesATunnelThatOnlyAccepts(t *testing.T) {
	closes := acceptAndDo(t, func(c net.Conn) { c.Close() })
	hangs := acceptAndDo(t, func(c net.Conn) { time.Sleep(10 * time.Second); c.Close() })
	for _, s := range []config.Service{config.ServiceCloudSQLMySQL, config.ServiceCloudSQL} {
		h := databaseHandshake(s, components.MySQLCredentials{Password: "p", RootPassword: "r"})
		if h == nil {
			t.Fatalf("%s has no handshake", s)
		}
		for name, addr := range map[string]string{"closes": closes, "hangs": hangs} {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			start := time.Now()
			err := h(ctx, addr)
			cancel()
			if err == nil {
				t.Errorf("%s through a listener that %s: ready", s, name)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("%s through a listener that %s took %s, past its 500ms deadline", s, name, d)
			}
		}
	}
}

// Only the databases have one: every other tunnel stays a TCP check.
func TestDatabaseHandshakeIsForTheDatabasesOnly(t *testing.T) {
	for _, s := range []config.Service{config.ServiceMemorystore, config.ServiceDatastore, config.ServiceStorage, config.ServiceBigQuery} {
		if databaseHandshake(s, components.MySQLCredentials{}) != nil {
			t.Errorf("%s has a database handshake", s)
		}
	}
}

// A MySQL server's error packet, the reply to a login it refuses, fails
// the handshake with the server's own words.
func TestMySQLHandshakeReportsTheServersError(t *testing.T) {
	addr := acceptAndDo(t, func(c net.Conn) {
		defer c.Close()
		// ERR packet, sequence 0: 1040 Too many connections.
		msg := "\xff\x10\x04#08004Too many connections"
		pkt := append([]byte{byte(len(msg)), 0, 0, 0}, msg...)
		_, _ = c.Write(pkt)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := mysqlHandshake(ctx, addr, components.MySQLCredentials{Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "Too many connections") {
		t.Errorf("mysqlHandshake = %v, want the server's 1040", err)
	}
}
