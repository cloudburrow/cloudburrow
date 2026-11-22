package sqladmin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Managed connection pooling (#2141), as Google documents it for
// PostgreSQL (https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling,
// read 2026-11-22): settings.connectionPoolConfig.connectionPoolingEnabled
// on a Cloud SQL Enterprise Plus instance runs a pooler beside the server,
// on port 6432, which the connectors and the Auth Proxy reach on 3307;
// 5432 stays the server's. Its flags are pool_mode (transaction, the
// default, or session) and the pooler's limits below; poolerCount, output
// only, is the instance's vCPUs divided by 4, at least 1. Enabling or
// disabling it restarts the instance. CloudBurrow's pooler is PgBouncer
// (ISC licence), whose settings these flags are; one pooler process
// serves however many poolerCount says (UNVERIFIED: Google runs that
// many, and max_client_connections is each one's).
//
// Cloud SQL for MySQL's managed connection pooling is a thread pool in the
// server (https://docs.cloud.google.com/sql/docs/mysql/managed-connection-pooling),
// which the community MySQL server CloudBurrow runs does not have: it is
// refused 501. The wording of every error here is UNVERIFIED (the page
// quotes none).

// PoolerPort is the pooler's port in the instance's pod: Google's 6432.
const PoolerPort = 6432

// poolFlag is one connectionPoolConfig flag: its PgBouncer setting and
// the values it takes.
type poolFlag struct {
	// setting is PgBouncer's name for it.
	setting string
	// values, when set, are the only values; otherwise it is a whole
	// number from min to max (max 0: no upper bound), or free text.
	values   []string
	min, max int64
	text     bool
	def      string
}

// poolFlags are Google's flags, with its defaults and ranges. Ranges the
// page does not give are any whole number from 0 (UNVERIFIED).
var poolFlags = map[string]poolFlag{
	"pool_mode":                      {setting: "pool_mode", values: []string{"transaction", "session"}, def: "transaction"},
	"max_pool_size":                  {setting: "default_pool_size", min: 1, def: "50"},
	"min_pool_size":                  {setting: "min_pool_size", def: "0"},
	"max_client_connections":         {setting: "max_client_conn", min: 1, def: "5000"},
	"max_prepared_statements":        {setting: "max_prepared_statements", def: "0"},
	"client_connection_idle_timeout": {setting: "client_idle_timeout", max: 2147483, def: "0"},
	"server_connection_idle_timeout": {setting: "server_idle_timeout", max: 2147483, def: "600"},
	"query_wait_timeout":             {setting: "query_wait_timeout", max: 2147483, def: "120"},
	"server_lifetime":                {setting: "server_lifetime", def: "3600"},
	"ignore_startup_parameters":      {setting: "ignore_startup_parameters", text: true, def: "extra_float_digits"},
}

// poolConfig is spec's settings.connectionPoolConfig.
func (s Spec) poolConfig() map[string]any {
	m, _ := s.Settings["connectionPoolConfig"].(map[string]any)
	return m
}

// PoolingEnabled reports whether spec runs the managed connection pooler.
func (s Spec) PoolingEnabled() bool {
	on, _ := s.poolConfig()["connectionPoolingEnabled"].(bool)
	return on && !s.MySQL() && !IsSQLServer(s.DatabaseVersion)
}

// PoolerSettings are the pooler's PgBouncer settings, each flag's value or
// its default, by PgBouncer's names.
func (s Spec) PoolerSettings() map[string]string {
	out := map[string]string{}
	for _, f := range poolFlags {
		out[f.setting] = f.def
	}
	for name, value := range s.poolFlagValues() {
		out[poolFlags[name].setting] = value
	}
	return out
}

// poolFlagValues are the flags spec sets, by Google's names.
func (s Spec) poolFlagValues() map[string]string {
	list, _ := s.poolConfig()["flags"].([]any)
	out := map[string]string{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		n, _ := m["name"].(string)
		v, _ := m["value"].(string)
		out[strings.TrimSpace(n)] = strings.TrimSpace(v)
	}
	return out
}

// poolingKey is what a change of which restarts the instance: the pooler
// on or off, and its settings.
func (s Spec) poolingKey() string {
	if !s.PoolingEnabled() {
		return ""
	}
	set := s.PoolerSettings()
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, set[k])
	}
	return b.String()
}

// poolerCount is Google's number of poolers: vCPUs / 4, at least 1.
func (s Spec) poolerCount() int64 {
	cores, _ := TierShape(s.Tier)
	n := int64(cores) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// checkConnectionPool checks spec's connectionPoolConfig.
func checkConnectionPool(spec Spec) error {
	pc := spec.poolConfig()
	if pc == nil {
		return nil
	}
	on, _ := pc["connectionPoolingEnabled"].(bool)
	if v, ok := pc["connectionPoolingEnabled"]; ok {
		if _, isBool := v.(bool); !isBool {
			return Errorf(http.StatusBadRequest, "Invalid value at 'instance.settings.connection_pool_config.connection_pooling_enabled' (TYPE_BOOL), %v", v)
		}
	}
	if list, ok := pc["flags"]; ok {
		if _, isList := list.([]any); !isList {
			return Errorf(http.StatusBadRequest, "Invalid value at 'instance.settings.connection_pool_config.flags': a list of {name, value}.")
		}
	}
	if !on {
		return nil
	}
	switch {
	case IsSQLServer(spec.DatabaseVersion):
		return Errorf(http.StatusBadRequest, "Invalid request: Managed Connection Pooling is not supported for SQL Server instances.")
	case spec.MySQL():
		return unimplemented("settings.connectionPoolConfig on Cloud SQL for MySQL",
			"Cloud SQL for MySQL's managed connection pooling is a server thread pool, which the community MySQL server run here does not have")
	case editionOf(spec) != "ENTERPRISE_PLUS":
		return Errorf(http.StatusBadRequest, "Invalid request: Managed Connection Pooling requires the Cloud SQL Enterprise Plus edition (settings.edition ENTERPRISE_PLUS).")
	}
	seen := map[string]bool{}
	list, _ := pc["flags"].([]any)
	for i, e := range list {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		value, _ := m["value"].(string)
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		f, ok := poolFlags[name]
		if !ok {
			return Errorf(http.StatusBadRequest, "Invalid request: connectionPoolConfig.flags[%d]: %q is not a Managed Connection Pooling flag.", i, name)
		}
		if seen[name] {
			return Errorf(http.StatusBadRequest, "Invalid request: connectionPoolConfig.flags[%d]: %s is set more than once.", i, name)
		}
		seen[name] = true
		if err := f.check(name, value); err != nil {
			return err
		}
	}
	return nil
}

func (f poolFlag) check(name, value string) error {
	switch {
	case len(f.values) > 0:
		for _, v := range f.values {
			if v == value {
				return nil
			}
		}
		return Errorf(http.StatusBadRequest, "Invalid request: Managed Connection Pooling flag %s takes %s, not %q.", name, strings.Join(f.values, " or "), value)
	case f.text:
		// A comma-separated list of startup parameter names.
		for _, p := range strings.Split(value, ",") {
			p = strings.TrimSpace(p)
			for _, r := range p {
				if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
					return Errorf(http.StatusBadRequest, "Invalid request: Managed Connection Pooling flag %s is a comma-separated list of parameter names, not %q.", name, value)
				}
			}
		}
		return nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < f.min || (f.max > 0 && n > f.max) {
		rng := fmt.Sprintf("a whole number from %d", f.min)
		if f.max > 0 {
			rng = fmt.Sprintf("a whole number from %d to %d", f.min, f.max)
		}
		return Errorf(http.StatusBadRequest, "Invalid request: Managed Connection Pooling flag %s takes %s, not %q.", name, rng, value)
	}
	return nil
}

// withPoolerCount answers settings.connectionPoolConfig.poolerCount,
// copying what it changes.
func withPoolerCount(st map[string]any, spec Spec) {
	pc, _ := st["connectionPoolConfig"].(map[string]any)
	if pc == nil {
		return
	}
	n := make(map[string]any, len(pc)+1)
	for k, v := range pc {
		n[k] = v
	}
	delete(n, "poolerCount")
	if spec.PoolingEnabled() {
		n["poolerCount"] = spec.poolerCount()
	}
	st["connectionPoolConfig"] = n
}
