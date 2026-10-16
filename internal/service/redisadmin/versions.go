package redisadmin

import (
	"context"
	"fmt"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Versions are the redis_version values a created instance runs (#1386),
// in upgrade order, each a server image whose INFO reports that version:
// Redis 6.2.24 (REDIS_6_X), Redis 7.0.15 (REDIS_7_0), both BSD-3-Clause,
// and Valkey 8.1 (REDIS_7_2, which reports Redis 7.2.4). Memorystore's
// other versions (REDIS_3_2, REDIS_4_0, REDIS_5_0) are 501: no image of
// them is served.
var Versions = []string{"REDIS_6_X", "REDIS_7_0", DefaultRedisVersion}

func versionRank(v string) int {
	for i, x := range Versions {
		if x == v {
			return i
		}
	}
	return -1
}

func checkVersion(v string) error {
	if v == "" || versionRank(v) >= 0 {
		return nil
	}
	switch v {
	case "REDIS_3_2", "REDIS_4_0", "REDIS_5_0":
		return apierror.Unimplemented("redis_version %s is not supported: CloudBurrow serves %v", v, Versions)
	}
	return apierror.InvalidArgument("redis_version %q is not a Memorystore for Redis version", v)
}

// version is r's redis_version.
func (r *record) version() string {
	if r.Version == "" {
		return DefaultRedisVersion
	}
	return r.Version
}

// UpgradeInstance moves a created instance to a later redis_version: its
// data is snapshotted (BGSAVE), its server started again on the version's
// image, and the snapshot loaded into it, as an import is. The instance is
// UPDATING until it is done. A downgrade is INVALID_ARGUMENT, as
// Memorystore refuses one.
func (s *Server) UpgradeInstance(ctx context.Context, req *redispb.UpgradeInstanceRequest) (*longrunningpb.Operation, error) {
	isDefault, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return nil, apierror.FailedPrecondition("instance %s is the server `cloudburrow up` runs, which cannot be upgraded; create an instance to upgrade one", req.GetName())
	}
	v := req.GetRedisVersion()
	if v == "" {
		return nil, apierror.InvalidArgument("redis_version is required")
	}
	if err := checkVersion(v); err != nil {
		return nil, err
	}
	c := s.created
	sh, ok := c.prov.(Shell)
	if !ok {
		return nil, apierror.Unimplemented("upgrading is not supported by this instance's servers")
	}
	c.mu.Lock()
	r := c.byID[req.GetName()]
	if versionRank(v) <= versionRank(r.version()) {
		cur := r.version()
		c.mu.Unlock()
		return nil, apierror.InvalidArgument("redis_version %s is not later than the instance's %s: an instance is only upgraded", v, cur)
	}
	op, err := c.claim(r, "upgrade")
	if err == nil {
		r.state = redispb.Instance_UPDATING
	}
	from := *r
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		ctx, cancel := c.cancellable(op)
		defer cancel()
		err := c.upgrade(ctx, sh, &from, v)
		if err != nil {
			// The server is as it was, or on the new image without the data:
			// say which, rather than READY on a guess.
			err = fmt.Errorf("upgrade to %s: %w", v, err)
		}
		c.done(from.Name, op, redispb.Instance_READY, err)
	}()
	return s.operation(op)
}

func (c *instances) upgrade(ctx context.Context, sh Shell, from *record, v string) error {
	data, err := c.snapshot(ctx, sh, from.dataServer(), from.addr, from.AuthString)
	if err != nil {
		return err
	}
	next := *from
	next.Version = v
	if from.ha() {
		// Every node on the new image, the primary where it was, then the
		// data loaded into the primary, which its replicas sync.
		if err := c.startHA(ctx, next); err != nil {
			return err
		}
		c.mu.Lock()
		addr := from.addr
		if r, ok := c.byID[from.Name]; ok {
			r.Version = v
			addr = r.addr
			_ = c.save(r)
		}
		c.mu.Unlock()
		return c.load(ctx, sh, next.dataServer(), next.binary(), addr, next.AuthString, data)
	}
	addr, err := c.prov.Provision(ctx, next.spec())
	if err != nil {
		return err
	}
	c.mu.Lock()
	if r, ok := c.byID[from.Name]; ok {
		r.addr, r.Version = addr, v
		_ = c.save(r)
	}
	c.mu.Unlock()
	return c.load(ctx, sh, next.Server, next.binary(), addr, next.AuthString, data)
}
