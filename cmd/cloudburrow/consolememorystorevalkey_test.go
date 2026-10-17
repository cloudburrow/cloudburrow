package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// fakeValkeyNodes runs cluster nodes in memory: every command is OK but
// CLUSTER MYID (the node's address), CLUSTER INFO (ok, knowing every node
// running) and INFO (a replica linked to its primary).
type fakeValkeyNodes struct {
	mu      sync.Mutex
	running map[string]string // server -> addr
	next    int
}

func (f *fakeValkeyNodes) StartClusterNode(_ context.Context, spec redisadmin.Spec) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	addr := fmt.Sprintf("127.0.0.1:%d", 30000+f.next)
	f.running[spec.Server] = addr
	return addr, fmt.Sprintf("10.0.0.%d", f.next), 30000 + f.next, nil
}

func (f *fakeValkeyNodes) Remove(_ context.Context, server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, server)
	return nil
}

type fakeValkeyNode struct {
	f    *fakeValkeyNodes
	addr string
}

func (n fakeValkeyNode) Do(_ context.Context, args ...any) (any, error) {
	cmd := strings.ToUpper(fmt.Sprint(args...))
	switch {
	case strings.HasPrefix(cmd, "CLUSTERMYID"):
		return n.addr, nil
	case strings.HasPrefix(cmd, "CLUSTERINFO"):
		n.f.mu.Lock()
		defer n.f.mu.Unlock()
		return fmt.Sprintf("cluster_state:ok\r\ncluster_known_nodes:%d\r\n", len(n.f.running)), nil
	case strings.HasPrefix(cmd, "INFO"):
		return "role:slave\r\nmaster_link_status:up\r\n", nil
	case strings.HasPrefix(cmd, "CLUSTERSHARDS"):
		return []any{}, nil
	}
	return "OK", nil
}

func (n fakeValkeyNode) Pipe(ctx context.Context, cmds [][]any) ([]any, error) {
	out := make([]any, len(cmds))
	for i, c := range cmds {
		out[i], _ = n.Do(ctx, c...)
	}
	return out, nil
}

func (fakeValkeyNode) Close() error { return nil }

type valkeyNoBackend struct{}

func (valkeyNoBackend) Info(context.Context) (redisadmin.ServerInfo, error) {
	return redisadmin.ServerInfo{}, fmt.Errorf("no server")
}

type valkeyNoProv struct{}

func (valkeyNoProv) Provision(context.Context, redisadmin.Spec) (string, error) {
	return "", fmt.Errorf("unused")
}
func (valkeyNoProv) Remove(context.Context, string) error { return nil }

// TestConsoleValkeyInstances (#1519): the Memorystore for Valkey page is
// reachable and drives the API: Create instance makes an instance in each
// mode, waiting for it to serve; the list shows its mode and endpoint; its
// page shows its endpoints and configuration and offers Edit, which adds a
// replica; Delete removes it.
func TestConsoleValkeyInstances(t *testing.T) {
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(context.Background(), valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
	nodes := &fakeValkeyNodes{running: map[string]string{}}
	srv.EnableValkey(context.Background(), nodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{nodes, addr} }, nil)
	p := valkeyProvider{server: func() *redisadmin.Server { return srv }}
	ctx := context.Background()

	if _, fields := p.CreateForm(); len(fields) == 0 {
		t.Fatal("no Create instance form")
	}
	for _, tc := range []struct {
		id, mode, shards, endpoint string
		tls                        bool
	}{
		{"vk-cluster", "CLUSTER", "2", "Discovery endpoint", true},
		{"vk-single", "CLUSTER_DISABLED", "1", "Primary endpoint", false},
	} {
		id, err := p.Create(ctx, "p", map[string]string{"id": tc.id, "region": redisadmin.Region, "mode": tc.mode, "shardCount": tc.shards,
			"replicaCount": "1", "nodeType": "SHARED_CORE_NANO", "network": "projects/p/global/networks/default",
			"engineConfigs": `{"maxmemory-policy":"allkeys-lru"}`, "transitEncryption": strconv.FormatBool(tc.tls), "iamAuth": strconv.FormatBool(tc.tls)})
		if err != nil || id != tc.id {
			t.Fatalf("Create %s = %q, %v", tc.id, id, err)
		}
		d, err := p.Detail(ctx, "p", []string{tc.id})
		if err != nil || d.Unavailable != "" || d.Edit == nil {
			t.Fatalf("Detail %s = %+v, %v", tc.id, d, err)
		}
		props := map[string]string{}
		for _, pr := range d.Sections[0].Groups[0].Properties {
			props[pr.Label] = pr.Value
		}
		if props["Mode"] != tc.mode || !strings.HasPrefix(props[tc.endpoint], "127.0.0.1:") || props["PSC networks"] != "projects/p/global/networks/default" ||
			props["Engine configs"] == "—" || props["State"] != "ACTIVE" {
			t.Errorf("%s's page shows %v", tc.id, props)
		}
		// In-transit encryption and IAM auth (#1549): the modes, and the
		// Server CA tab with the instance's CA.
		hasCA := len(d.Sections) > 1 && d.Sections[1].ID == "server-ca" && strings.Contains(d.Sections[1].Text, "BEGIN CERTIFICATE")
		if tc.tls && (props["In-transit encryption"] != "SERVER_AUTHENTICATION" || props["Authorization"] != "IAM_AUTH" || !hasCA) {
			t.Errorf("%s with in-transit encryption: page shows %v, Server CA tab %v", tc.id, props, hasCA)
		}
		if !tc.tls && hasCA {
			t.Errorf("%s without in-transit encryption has a Server CA tab", tc.id)
		}
		if err := p.Edit(ctx, "p", []string{tc.id}, map[string]string{"shardCount": tc.shards, "replicaCount": "2", "nodeType": "SHARED_CORE_NANO"}); err != nil {
			t.Fatalf("Edit %s: %v", tc.id, err)
		}
	}
	l, err := p.List(ctx, "p")
	if err != nil || l.Unavailable != "" || len(l.Items) != 2 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	for _, it := range l.Items {
		if it.Fields["Replicas"] != "2" || !strings.HasPrefix(it.Fields["Endpoint"], "127.0.0.1:") {
			t.Errorf("listed %s as %v", it.Name, it.Fields)
		}
	}
	for _, id := range []string{"vk-cluster", "vk-single"} {
		if err := p.Delete(ctx, "p", id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	if l, _ := p.List(ctx, "p"); len(l.Items) != 0 {
		t.Errorf("after Delete the list is %+v", l.Items)
	}
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	if len(nodes.running) != 0 {
		t.Errorf("after Delete %d nodes still run", len(nodes.running))
	}
}
