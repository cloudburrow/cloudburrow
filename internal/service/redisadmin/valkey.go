package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey is the Backend for the server at an address, reached with an
// ordinary Redis client, one short-lived connection per call.
type Valkey struct {
	// Addr returns the server's address now: the tunnel's host address,
	// which is known only once the tunnel is up.
	Addr func() string
	// Password is the server's AUTH string, "" for none.
	Password string
	// CA is the PEM CA of a server with in-transit encryption (#1413),
	// which is reached over TLS; "" for plain TCP.
	CA string
}

// Dial is the Dialer of a created instance's server.
func Dial(addr, password, caPEM string) DataPlane {
	return Valkey{Addr: func() string { return addr }, Password: password, CA: caPEM}
}

func (v Valkey) client() (*redis.Client, error) {
	addr := ""
	if v.Addr != nil {
		addr = v.Addr()
	}
	if addr == "" {
		return nil, errors.New("the Memorystore tunnel has no address")
	}
	tc, err := ClientTLS(v.CA)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{Addr: addr, Password: v.Password, MaxRetries: -1, PoolSize: 1, TLSConfig: tc}), nil
}

// Info reads INFO server's redis_version and CONFIG GET maxmemory.
func (v Valkey) Info(ctx context.Context) (ServerInfo, error) {
	c, err := v.client()
	if err != nil {
		return ServerInfo{}, err
	}
	defer c.Close()
	raw, err := c.Info(ctx, "server").Result()
	if err != nil {
		return ServerInfo{}, err
	}
	var info ServerInfo
	for _, line := range strings.Split(raw, "\n") {
		if k, val, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "redis_version" {
			info.Version = val
		}
	}
	cfg, err := c.ConfigGet(ctx, "maxmemory").Result()
	if err != nil {
		return ServerInfo{}, err
	}
	if n, err := strconv.ParseInt(cfg["maxmemory"], 10, 64); err == nil {
		info.MaxMemory = n
	}
	return info, nil
}

// Flush empties every database, as FLUSHALL does: the console's Flush.
// The admin API has no flush method; this is the data plane.
func (v Valkey) Flush(ctx context.Context) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.FlushAll(ctx).Err()
}

// ConfigSet changes settings of the running server, one CONFIG SET each,
// so the server's own words say which it refused.
func (v Valkey) ConfigSet(ctx context.Context, settings map[string]string) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	defer c.Close()
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := c.ConfigSet(ctx, k, settings[k]).Err(); err != nil {
			return fmt.Errorf("CONFIG SET %s %q: %w", k, settings[k], err)
		}
	}
	return nil
}

// infoField is field of INFO section, "" when absent.
func infoField(ctx context.Context, c *redis.Client, section, field string) (string, error) {
	raw, err := c.Info(ctx, section).Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(raw, "\n") {
		if k, val, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == field {
			return val, nil
		}
	}
	return "", nil
}

// savePoll is how often Save and Load read the server's progress.
var savePoll = 100 * time.Millisecond

// Save snapshots the dataset with BGSAVE (#1384), waits for it to finish,
// and returns the RDB file's path on the server's file system.
func (v Valkey) Save(ctx context.Context) (string, error) {
	c, err := v.client()
	if err != nil {
		return "", err
	}
	defer c.Close()
	for {
		// BGSAVE forks before it answers, so once it has answered the
		// snapshot is in progress until rdb_bgsave_in_progress is 0.
		err := c.BgSave(ctx).Err()
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "in progress") {
			return "", fmt.Errorf("BGSAVE: %w", err)
		}
		// Another save or an AOF rewrite is running: wait for it.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(savePoll):
		}
	}
	for {
		busy, err := infoField(ctx, c, "persistence", "rdb_bgsave_in_progress")
		if err != nil {
			return "", err
		}
		if busy == "0" {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(savePoll):
		}
	}
	if st, err := infoField(ctx, c, "persistence", "rdb_last_bgsave_status"); err != nil {
		return "", err
	} else if st != "ok" {
		return "", fmt.Errorf("BGSAVE failed: rdb_last_bgsave_status is %s", st)
	}
	cfg, err := c.ConfigGet(ctx, "dir").Result()
	if err != nil {
		return "", err
	}
	file, err := c.ConfigGet(ctx, "dbfilename").Result()
	if err != nil {
		return "", err
	}
	return path.Join(cfg["dir"], file["dbfilename"]), nil
}

// Load replaces the dataset with the one of the server at host:port, in
// the same pod (#1384): the server replicates it, which empties its own
// data and loads the other's RDB, then stops replicating. An AOF, when the
// server keeps one, is written again from the loaded data.
func (v Valkey) Load(ctx context.Context, host string, port int) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if v.CA != "" {
		// The server at host:port, in the same pod, speaks plain TCP.
		if err := c.ConfigSet(ctx, "tls-replication", "no").Err(); err != nil {
			return fmt.Errorf("CONFIG SET tls-replication no: %w", err)
		}
		defer c.ConfigSet(context.WithoutCancel(ctx), "tls-replication", "yes")
	}
	if err := c.Do(ctx, "REPLICAOF", host, strconv.Itoa(port)).Err(); err != nil {
		return fmt.Errorf("REPLICAOF: %w", err)
	}
	defer c.Do(context.WithoutCancel(ctx), "REPLICAOF", "NO", "ONE")
	for {
		link, err := infoField(ctx, c, "replication", "master_link_status")
		if err != nil && !strings.HasPrefix(err.Error(), "LOADING") {
			return err
		}
		syncing, _ := infoField(ctx, c, "replication", "master_sync_in_progress")
		if err == nil && link == "up" && syncing == "0" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server did not load the RDB file: %w", ctx.Err())
		case <-time.After(savePoll):
		}
	}
	return c.Do(ctx, "REPLICAOF", "NO", "ONE").Err()
}

// ReplicaOf makes the server a replica of host:port, or a primary with
// host "" (REPLICAOF NO ONE): a Standard tier node (#1385).
func (v Valkey) ReplicaOf(ctx context.Context, host string, port int) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if host == "" {
		return c.Do(ctx, "REPLICAOF", "NO", "ONE").Err()
	}
	return c.Do(ctx, "REPLICAOF", host, strconv.Itoa(port)).Err()
}

// Replication reads INFO replication: the server's role, how much of the
// primary's stream it has, and whether a replica's link is up.
func (v Valkey) Replication(ctx context.Context) (Replication, error) {
	c, err := v.client()
	if err != nil {
		return Replication{}, err
	}
	defer c.Close()
	raw, err := c.Info(ctx, "replication").Result()
	if err != nil {
		return Replication{}, err
	}
	return parseReplication(raw), nil
}

func parseReplication(raw string) Replication {
	f := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if k, val, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			f[k] = val
		}
	}
	st := Replication{Primary: f["role"] == "master"}
	key := "master_repl_offset"
	if !st.Primary {
		key = "slave_repl_offset"
		st.LinkUp = f["master_link_status"] == "up"
	}
	st.Offset, _ = strconv.ParseInt(f[key], 10, 64)
	return st
}

// DialCluster is the ClusterDialer of a cluster's nodes: a go-redis client
// of one node, not following redirections, since the cluster is being
// formed or changed through each node in turn.
func DialCluster(addr, password string) ClusterNode {
	return clusterNodeClient{redis.NewClient(&redis.Options{Addr: addr, Password: password, MaxRetries: -1, PoolSize: 1})}
}

type clusterNodeClient struct{ c *redis.Client }

func (n clusterNodeClient) Do(ctx context.Context, args ...any) (any, error) {
	return n.c.Do(ctx, args...).Result()
}

func (n clusterNodeClient) Pipe(ctx context.Context, cmds [][]any) ([]any, error) {
	p := n.c.Pipeline()
	out := make([]*redis.Cmd, len(cmds))
	for i, args := range cmds {
		out[i] = p.Do(ctx, args...)
	}
	if _, err := p.Exec(ctx); err != nil {
		return nil, err
	}
	res := make([]any, len(out))
	for i, c := range out {
		res[i] = c.Val()
	}
	return res, nil
}

func (n clusterNodeClient) Close() error { return n.c.Close() }
