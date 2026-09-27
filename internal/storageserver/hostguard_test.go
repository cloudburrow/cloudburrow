package storageserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// The builtin storage server, started with the arguments its in-cluster
// Deployment gets, refuses a rebound attacker domain on its JSON API and
// answers every name a real caller uses (#676): the tunnel from the host,
// virtual-hosted buckets from the host and from pods, the Service name
// pods are given and its shorter forms, and addresses (a kubelet probe, a
// cluster IP).
func TestStorageServerRefusesForeignHosts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	args := append(components.BuiltinStorageBackend("cb", "unused", false, false, nil, nil).Args, "--listen", addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, args, io.Discard, io.Discard) }()
	defer func() {
		http.DefaultClient.CloseIdleConnections()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	base := "http://" + addr
	for i := 0; ; i++ {
		resp, err := http.Get(base + "/storage/v1/b?project=p")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i == 100 {
			t.Fatalf("the server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	req := func(method, path, host, body string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		r.Host = host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://"+host)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := req("POST", "/storage/v1/b?project=p", "127.0.0.1:9001", `{"name":"rebind"}`); code != http.StatusOK {
		t.Fatalf("create through the tunnel's address = %d %s", code, body)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/storage/v1/b?project=p"},
		{"GET", "/storage/v1/b/rebind/o"},
		{"POST", "/upload/storage/v1/b/rebind/o?uploadType=media&name=x"},
		{"DELETE", "/storage/v1/b/rebind"},
		{"GET", "/rebind/x"},
	} {
		code, body := req(c.method, c.path, "attacker.example:9001", "{}")
		if code != http.StatusMisdirectedRequest || !strings.Contains(body, "attacker.example:9001") {
			t.Errorf("%s %s with Host attacker.example:9001 = %d %.200q, want 421 naming the host", c.method, c.path, code, body)
		}
	}
	for _, host := range []string{
		"127.0.0.1:9001", "localhost:9001", "[::1]:9001",
		"storage.cb.svc.cluster.local:4443", "storage.cb.svc:4443", "storage:4443",
		"10.244.0.9:4443", "10.96.12.4:4443",
	} {
		if code, body := req("GET", "/storage/v1/b/rebind", host, ""); code != http.StatusOK {
			t.Errorf("GET a bucket with Host %s = %d %.200s, want 200", host, code, body)
		}
	}
	// Virtual-hosted XML: the bucket is in the name, so the path is the key.
	for _, host := range []string{"rebind.storage.localhost:9001", "rebind.localhost:9001", "rebind.storage.cb.svc.cluster.local:4443"} {
		if code, body := req("GET", "/", host, ""); code != http.StatusOK || !strings.Contains(body, "<Name>rebind</Name>") {
			t.Errorf("virtual-hosted list with Host %s = %d %.200s, want the rebind bucket", host, code, body)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	got := strings.Join(AllowedHosts([]string{"storage.cb.svc.cluster.local", "storage.localhost"}), " ")
	want := "storage.cb.svc.cluster.local *.storage.cb.svc.cluster.local storage storage.cb.svc storage.localhost *.storage.localhost"
	if got != want {
		t.Errorf("AllowedHosts = %s\nwant %s", got, want)
	}
}
