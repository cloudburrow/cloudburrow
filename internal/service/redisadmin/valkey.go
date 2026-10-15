package redisadmin

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Valkey is the Backend for the server at an address, reached with an
// ordinary Redis client, one short-lived connection per call.
type Valkey struct {
	// Addr returns the server's address now: the tunnel's host address,
	// which is known only once the tunnel is up.
	Addr func() string
}

func (v Valkey) client() (*redis.Client, error) {
	addr := ""
	if v.Addr != nil {
		addr = v.Addr()
	}
	if addr == "" {
		return nil, errors.New("the Memorystore tunnel has no address")
	}
	return redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, PoolSize: 1}), nil
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
