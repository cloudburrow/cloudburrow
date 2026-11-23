package alloydb

// Managed connection pooling (#2191), as Google documents it
// (docs.cloud.google.com/alloydb/docs/configure-managed-connection-pooling
// and the REST reference's Instance.connectionPoolConfig, read
// 2026-11-23): connection_pool_config.enabled on a primary or read pool
// instance runs a pooler beside it, which clients reach on port 6432 and
// the connectors and the Auth Proxy reach through port 5433, in a pool of
// their own; its flags are those below, with Google's defaults and
// ranges; pooler_count is output only. The instance's SSL mode applies to
// it. CloudBurrow's pooler is PgBouncer (ISC licence), whose settings
// these flags are, as Cloud SQL's is (#2141).

import (
	"strconv"
	"strings"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// PoolerPort is the pooler's port: Google's 6432.
const PoolerPort = 6432

type poolFlag struct {
	setting  string
	values   []string
	min, max int64
	list     bool
	def      string
}

const maxPoolCount = 262042
const maxPoolSeconds = 2147483

// poolFlags are Google's flags, by Google's name, with PgBouncer's.
var poolFlags = map[string]poolFlag{
	"pool_mode":                      {setting: "pool_mode", values: []string{"transaction", "session"}, def: "transaction"},
	"max_pool_size":                  {setting: "default_pool_size", min: 1, max: maxPoolCount, def: "50"},
	"min_pool_size":                  {setting: "min_pool_size", max: maxPoolCount, def: "0"},
	"max_client_connections":         {setting: "max_client_conn", min: 1, max: maxPoolCount, def: "5000"},
	"client_connection_idle_timeout": {setting: "client_idle_timeout", max: maxPoolSeconds, def: "0"},
	"server_connection_idle_timeout": {setting: "server_idle_timeout", max: maxPoolSeconds, def: "600"},
	"query_wait_timeout":             {setting: "query_wait_timeout", max: maxPoolSeconds, def: "120"},
	"max_prepared_statements":        {setting: "max_prepared_statements", max: maxPoolCount, def: "0"},
	"ignore_startup_parameters":      {setting: "ignore_startup_parameters", list: true, def: "extra_float_digits"},
	"server_lifetime":                {setting: "server_lifetime", max: maxPoolSeconds, def: "3600"},
	"stats_users":                    {setting: "stats_users", list: true},
}

// checkPooling checks an instance's connection_pool_config. UNVERIFIED:
// Google's messages.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/configure-managed-connection-pooling.)
func checkPooling(in *alloydbpb.Instance) error {
	pc := in.GetConnectionPoolConfig()
	if pc == nil {
		return nil
	}
	if pc.GetEnabled() && in.GetInstanceType() == alloydbpb.Instance_SECONDARY {
		return apierror.InvalidArgument("managed connection pooling is for primary and read pool instances, not secondary instances")
	}
	for name, value := range pc.GetFlags() {
		f, ok := poolFlags[name]
		if !ok {
			return apierror.InvalidArgument("connection_pool_config.flags: %q is not a managed connection pooling flag", name)
		}
		value = strings.TrimSpace(value)
		switch {
		case len(f.values) > 0:
			ok := false
			for _, v := range f.values {
				ok = ok || v == value
			}
			if !ok {
				return apierror.InvalidArgument("connection_pool_config.flags: %s takes %s, not %q", name, strings.Join(f.values, " or "), value)
			}
		case f.list:
			for _, p := range strings.Split(value, ",") {
				for _, r := range strings.TrimSpace(p) {
					if !(r == '_' || r == '-' || r == '.' || r == '@' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
						return apierror.InvalidArgument("connection_pool_config.flags: %s is a comma-separated list of names, not %q", name, value)
					}
				}
			}
		default:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < f.min || (f.max > 0 && n > f.max) {
				return apierror.InvalidArgument("connection_pool_config.flags: %s must be a whole number from %d to %d, not %q", name, f.min, f.max, value)
			}
		}
	}
	if pc.GetFlags()["min_pool_size"] != "" && pc.GetFlags()["max_pool_size"] != "" {
		lo, _ := strconv.ParseInt(pc.GetFlags()["min_pool_size"], 10, 64)
		hi, _ := strconv.ParseInt(pc.GetFlags()["max_pool_size"], 10, 64)
		if lo > hi {
			return apierror.InvalidArgument("connection_pool_config.flags: min_pool_size %d is more than max_pool_size %d", lo, hi)
		}
	}
	return nil
}

// stampPooling sets connection_pool_config's output fields: pooler_count,
// the instance's vCPUs / 4, at least 1 (UNVERIFIED: Google's sizing,
// POOLER_MACHINE_SIZED; Cloud SQL documents this one).
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/configure-managed-connection-pooling.)
func stampPooling(in *alloydbpb.Instance) {
	pc := in.GetConnectionPoolConfig()
	if pc == nil {
		return
	}
	pc.PoolerCount = 0
	if pc.GetEnabled() {
		n := in.GetMachineConfig().GetCpuCount() / 4
		if n < 1 {
			n = 1
		}
		pc.PoolerCount = n
	}
}

// poolerOf is the pooler's PgBouncer settings when in pools connections,
// each flag's value or its default; nil when it does not.
func poolerOf(in *alloydbpb.Instance) map[string]string {
	pc := in.GetConnectionPoolConfig()
	if !pc.GetEnabled() || in.GetInstanceType() == alloydbpb.Instance_SECONDARY {
		return nil
	}
	out := map[string]string{}
	for _, f := range poolFlags {
		if f.def != "" {
			out[f.setting] = f.def
		}
	}
	for name, value := range pc.GetFlags() {
		out[poolFlags[name].setting] = strings.TrimSpace(value)
	}
	return out
}
