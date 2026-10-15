package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	// Password is the server's AUTH string, "" for none.
	Password string
}

// Dial is the Dialer of a created instance's server.
func Dial(addr, password string) DataPlane {
	return Valkey{Addr: func() string { return addr }, Password: password}
}

func (v Valkey) client() (*redis.Client, error) {
	addr := ""
	if v.Addr != nil {
		addr = v.Addr()
	}
	if addr == "" {
		return nil, errors.New("the Memorystore tunnel has no address")
	}
	return redis.NewClient(&redis.Options{Addr: addr, Password: v.Password, MaxRetries: -1, PoolSize: 1}), nil
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
