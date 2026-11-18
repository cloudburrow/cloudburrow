package redisadmin

import (
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// configDefaults are the redis_configs a created instance accepts, each
// with the value it has when not set: Memorystore's defaults, which are
// Valkey's own but for maxmemory-policy (volatile-lru, as the instance
// `up` runs). maxmemory-gb is Memorystore's own, and becomes maxmemory.
var configDefaults = map[string]string{
	"maxmemory-policy":        "volatile-lru",
	"notify-keyspace-events":  "",
	"activedefrag":            "no",
	"lfu-decay-time":          "1",
	"lfu-log-factor":          "10",
	"stream-node-max-bytes":   "4096",
	"stream-node-max-entries": "100",
}

// maxmemoryGB is Memorystore's config for a maxmemory below the size.
const maxmemoryGB = "maxmemory-gb"

var policies = map[string]bool{"noeviction": true, "allkeys-lru": true, "volatile-lru": true, "allkeys-random": true,
	"volatile-random": true, "volatile-ttl": true, "volatile-lfu": true, "allkeys-lfu": true}

// configSince is the first redis_version each config is documented for
// (docs.cloud.google.com/memorystore/docs/redis/supported-redis-configurations,
// read 2026-11-17, #1524); one not here is on every version.
var configSince = map[string]string{
	"activedefrag": "REDIS_4_0", "lfu-decay-time": "REDIS_4_0", "lfu-log-factor": "REDIS_4_0",
	"stream-node-max-bytes": "REDIS_5_0", "stream-node-max-entries": "REDIS_5_0",
}

// lfuPolicies are the eviction policies of Redis 4.0 and later.
var lfuPolicies = map[string]bool{"volatile-lfu": true, "allkeys-lfu": true}

// checkConfigs refuses a config Memorystore does not accept, a value that
// is not one, and one the instance's version (redis_version, "" for the
// default) does not have, before any server sees it.
func checkConfigs(configs map[string]string, sizeGB int32, version string) error {
	for k, v := range configs {
		if since, ok := configSince[k]; ok && !atLeast(version, since) {
			return apierror.InvalidArgument("redis_configs %q is not available on %s: it needs Redis %s or later", k, version, versionNumber(since))
		}
		if k == "maxmemory-policy" && lfuPolicies[v] && !atLeast(version, "REDIS_4_0") {
			return apierror.InvalidArgument("redis_configs %s=%q is not available on %s: it needs Redis 4.0 or later", k, v, version)
		}
		switch k {
		case maxmemoryGB:
			gb, err := strconv.ParseFloat(v, 64)
			if err != nil || gb <= 0 || gb > float64(sizeGB) {
				return apierror.InvalidArgument("redis_configs %s=%q must be a number of GB above 0 and at most memory_size_gb (%d)", k, v, sizeGB)
			}
		case "maxmemory-policy":
			if !policies[v] {
				return apierror.InvalidArgument("redis_configs %s=%q is not an eviction policy", k, v)
			}
		case "activedefrag":
			if v != "yes" && v != "no" {
				return apierror.InvalidArgument("redis_configs %s=%q must be yes or no", k, v)
			}
		case "lfu-decay-time", "lfu-log-factor", "stream-node-max-bytes", "stream-node-max-entries":
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				return apierror.InvalidArgument("redis_configs %s=%q must be a non-negative integer", k, v)
			}
		case "notify-keyspace-events":
		default:
			return apierror.InvalidArgument("redis_configs %q is not a config Memorystore accepts", k)
		}
	}
	return nil
}

// serverConfigs are configs as the server's own settings: all but
// maxmemory-gb, which maxMemory reads.
func serverConfigs(configs map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range configs {
		if k != maxmemoryGB {
			out[k] = v
		}
	}
	return out
}

// maxMemory is the server's maxmemory in bytes: maxmemory-gb when set,
// else the instance's size.
func maxMemory(sizeGB int32, configs map[string]string) int64 {
	const gib = 1 << 30
	if v, ok := configs[maxmemoryGB]; ok {
		if gb, err := strconv.ParseFloat(v, 64); err == nil && gb > 0 {
			return int64(gb * gib)
		}
	}
	return int64(sizeGB) * gib
}

// versionNumber is a redis_version as Redis numbers it: REDIS_4_0 is 4.0,
// REDIS_6_X 6.x.
func versionNumber(v string) string {
	return strings.ReplaceAll(strings.TrimPrefix(v, "REDIS_"), "_", ".")
}
