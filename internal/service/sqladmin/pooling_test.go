package sqladmin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// TestConnectionPoolConfig (#2141): Google's flags, defaults and ranges,
// Enterprise Plus PostgreSQL only, poolerCount vCPUs / 4, and the pooler's
// PgBouncer settings in the server's pod.
func TestConnectionPoolConfig(t *testing.T) {
	spec := func(version, edition, tier string, flags ...string) Spec {
		var list []any
		for i := 0; i+1 < len(flags); i += 2 {
			list = append(list, map[string]any{"name": flags[i], "value": flags[i+1]})
		}
		return Spec{DatabaseVersion: version, Tier: tier, Settings: map[string]any{"edition": edition,
			"connectionPoolConfig": map[string]any{"connectionPoolingEnabled": true, "flags": list}}}
	}
	for _, c := range []struct {
		s    Spec
		code int
	}{
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2"), 0},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "pool_mode", "session", "max_pool_size", "10", "query_wait_timeout", "2147483"), 0},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "ignore_startup_parameters", "extra_float_digits,options"), 0},
		{spec("POSTGRES_15", "ENTERPRISE", "db-custom-2-7680"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "pool_mode", "statement"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "max_pool_size", "0"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "server_connection_idle_timeout", "2147484"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "auth_type", "trust"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "ignore_startup_parameters", "a\nauth_type = trust"), http.StatusBadRequest},
		{spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-2", "max_pool_size", "5", "max_pool_size", "6"), http.StatusBadRequest},
		{spec("MYSQL_8_4", "ENTERPRISE_PLUS", "db-perf-optimized-N-2"), http.StatusNotImplemented},
	} {
		err := checkConnectionPool(c.s)
		code := 0
		if e, ok := err.(*apiError); ok {
			code = e.code
		} else if err != nil {
			code = -1
		}
		if code != c.code {
			t.Errorf("%s %v: %v; want %d", c.s.DatabaseVersion, c.s.Settings, err, c.code)
		}
	}
	for tier, want := range map[string]int64{"db-perf-optimized-N-2": 1, "db-perf-optimized-N-4": 1, "db-perf-optimized-N-8": 2, "db-perf-optimized-N-64": 16} {
		if got := spec("POSTGRES_17", "ENTERPRISE_PLUS", tier).poolerCount(); got != want {
			t.Errorf("poolerCount of %s = %d, want %d", tier, got, want)
		}
	}
	s := spec("POSTGRES_17", "ENTERPRISE_PLUS", "db-perf-optimized-N-8", "max_pool_size", "2", "pool_mode", "session")
	set := s.PoolerSettings()
	if set["default_pool_size"] != "2" || set["pool_mode"] != "session" || set["max_client_conn"] != "5000" || set["server_idle_timeout"] != "600" {
		t.Errorf("PoolerSettings %v", set)
	}
	m := components.Backend{Name: "sqlinst-x", Image: "postgres", Port: 5432,
		Sidecars: []components.Sidecar{components.CloudSQLPooler(PoolerPort, set)}}.Manifest("ns", "i")
	for _, want := range []string{"- name: pooler\n", "image: " + components.PgBouncerImage, "containerPort: 6432", "port: 6432\n      targetPort: 6432", "default_pool_size = 2"} {
		if !strings.Contains(m, want) {
			t.Errorf("the manifest has no %q:\n%s", want, m)
		}
	}
	off := s
	off.Settings = map[string]any{"connectionPoolConfig": map[string]any{"connectionPoolingEnabled": false}}
	if off.PoolingEnabled() || off.poolingKey() == s.poolingKey() {
		t.Error("turning pooling off is not a change that restarts the instance")
	}
}
