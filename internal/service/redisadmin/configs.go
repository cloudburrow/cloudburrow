package redisadmin

import (
	"strconv"

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

// checkConfigs refuses a config Memorystore does not accept, and a value
// that is not one, before any server sees it.
func checkConfigs(configs map[string]string, sizeGB int32) error {
	for k, v := range configs {
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
