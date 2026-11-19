package redisadmin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// cfNodes are fake cluster-mode nodes by address: each answers CLUSTER
// MYID and ROLE, or, down, refuses the connection.
type cfNodes struct {
	mu      sync.Mutex
	id      map[string]string
	primary map[string]bool
	down    map[string]bool
}

type cfConn struct {
	f    *cfNodes
	addr string
}

func (c cfConn) Do(ctx context.Context, args ...any) (any, error) {
	out, err := c.Pipe(ctx, [][]any{args})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (c cfConn) Pipe(_ context.Context, cmds [][]any) ([]any, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.f.down[c.addr] {
		return nil, errors.New("connection refused")
	}
	out := make([]any, len(cmds))
	for i, cmd := range cmds {
		switch cmd[0] {
		case "CLUSTER":
			out[i] = c.f.id[c.addr]
		case "ROLE":
			if c.f.primary[c.addr] {
				out[i] = []any{"master", int64(10), []any{}}
			} else {
				out[i] = []any{"slave", "10.0.0.1", int64(6379), "connected", int64(10)}
			}
		}
	}
	return out, nil
}

func (cfConn) Close() error { return nil }

// TestClusterFailoverPrimaryLostBeforeFirstCheck (#1910): a cluster's
// nodes are known by the IDs they gave when the cluster was formed, so a
// primary that is lost before the watcher's first check of the cluster,
// whether it stays down or comes back with a new identity, is still found
// failed. In the full memorystore shard the test's pod deletion came
// before the first check, and the cluster was never failed over: the
// checks waited for the node to answer as itself first, which it never
// would again.
func TestClusterFailoverPrimaryLostBeforeFirstCheck(t *testing.T) {
	for _, tc := range []struct {
		what   string
		lose   func(f *cfNodes)
		checks int
	}{
		{"down", func(f *cfNodes) { f.down["p:1"] = true }, failAfter},
		{"started again", func(f *cfNodes) { f.id["p:1"] = "new-id" }, 1},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f := &cfNodes{
				id:      map[string]string{"p:1": "primary-id", "r:1": "replica-id"},
				primary: map[string]bool{"p:1": true},
				down:    map[string]bool{},
			}
			name := "projects/p/locations/us-central1/clusters/fo"
			c := &clusters{
				dial:   func(addr, _, _ string) ClusterNode { return cfConn{f, addr} },
				byName: map[string]*clusterRecord{},
				busy:   map[string]string{},
			}
			c.byName[name] = &clusterRecord{Name: name, ShardCount: 1, ReplicaCount: 1, state: clusterpb.Cluster_ACTIVE,
				Nodes: []*clusterNode{
					{Server: "fo-0", Shard: 0, Primary: true, addr: "p:1", id: "primary-id"},
					{Server: "fo-1", Shard: 0, addr: "r:1", id: "replica-id"},
				}}
			// The primary is lost before the first check.
			tc.lose(f)
			hs := map[string]*nodeHealth{}
			due := false
			for i := 0; i < tc.checks; i++ {
				due = c.checkCluster(context.Background(), name, hs, 10*time.Millisecond)
			}
			if !due {
				t.Fatalf("after %d checks of a cluster whose primary was lost before the first, no repair is due", tc.checks)
			}
		})
	}
}
