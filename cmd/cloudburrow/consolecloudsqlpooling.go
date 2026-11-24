package main

// Managed connection pooling in Cloud SQL's console (#2141), as Google's
// page has it (https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling,
// re-read 2026-11-22): on the create page and in Edit, Connections' Enable
// Managed Connection Pool, then Managed connection pool's Advanced
// pooling options (the pool mode and the pooler flags); the Overview's
// Connect to this instance shows whether the instance pools connections.
// The options' labels are the flags' names, as the page lists them
// (their console labels are UNVERIFIED, re-read 2026-11-22). Cloud SQL for MySQL's
// (https://docs.cloud.google.com/sql/docs/mysql/managed-connection-pooling)
// is a thread pool whose one option is max_pool_size, its default by the
// instance's vCPUs; clients connect on 3306 as before.

import (
	"strconv"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// sqlPoolFlagFields are the Advanced pooling options' numeric flags, with
// Google's defaults.
var sqlPoolFlagFields = []struct{ name, def, help string }{
	{"max_pool_size", "50", "Server connections per database and user pair."},
	{"min_pool_size", "0", "Server connections kept open per pool."},
	{"max_client_connections", "5000", "Client connections per pooler."},
	{"max_prepared_statements", "0", "Prepared statements tracked per connection in transaction mode; 0 turns their support off."},
	{"client_connection_idle_timeout", "0", "Seconds before an idle client connection is closed, 0 to 2147483; 0 never."},
	{"server_connection_idle_timeout", "600", "Seconds before an idle server connection is closed, 0 to 2147483."},
	{"query_wait_timeout", "120", "Seconds a query may wait for a server connection, 0 to 2147483; 0 waits indefinitely."},
	{"server_lifetime", "3600", "Seconds before a server connection is closed after use; 0 closes it at once."},
}

// sqlPoolFields are Enable Managed Connection Pool and its options, shown
// while show holds, set from cur.
func sqlPoolFields(section string, show map[string]string, cur *sqladminapi.ConnectionPoolConfig) []console.Field {
	on, vals := "false", map[string]string{}
	if cur != nil {
		if cur.ConnectionPoolingEnabled {
			on = "true"
		}
		for _, f := range cur.Flags {
			vals[f.Name] = f.Value
		}
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range show {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	opts := with(map[string]string{"connectionPooling": "true"})
	out := []console.Field{
		{Name: "connectionPooling", Label: "Enable Managed Connection Pool", Type: "checkbox", Section: section, Default: on, VisibleWhen: with(nil),
			Help: "Runs a connection pooler beside the server (PgBouncer here): clients connect to it on port 6432, and the Cloud SQL connectors and the Auth Proxy on 3307; 5432 stays the server's. Enterprise Plus only. Turning it on or off restarts the instance."},
		{Name: "poolMode", Label: "Pool mode", Type: "radio", Section: section, Panel: "Advanced pooling options", VisibleWhen: opts,
			Default: vpcOr(vals["pool_mode"], "transaction"), Options: []string{"transaction", "session"},
			OptionLabels: map[string]string{"transaction": "Transaction", "session": "Session"},
			Help:         "Transaction returns a server connection to the pool after each transaction; Session keeps it for the client's session."},
	}
	for _, f := range sqlPoolFlagFields {
		out = append(out, console.Field{Name: "pool_" + f.name, Label: f.name, Type: "number", Section: section, Panel: "Advanced pooling options",
			VisibleWhen: opts, Default: vpcOr(vals[f.name], f.def), Help: f.help})
	}
	out = append(out, console.Field{Name: "pool_ignore_startup_parameters", Label: "ignore_startup_parameters", Type: "text", Section: section,
		Panel: "Advanced pooling options", VisibleWhen: opts, Default: vals["ignore_startup_parameters"],
		Help: "Startup parameters the pooler ignores, separated by commas, such as extra_float_digits."})
	return out
}

// sqlMySQLPoolFields are Cloud SQL for MySQL's Enable Managed Connection
// Pool and its max_pool_size, shown while show holds, set from cur.
func sqlMySQLPoolFields(section string, show map[string]string, cur *sqladminapi.ConnectionPoolConfig) []console.Field {
	on, size := "false", ""
	if cur != nil {
		if cur.ConnectionPoolingEnabled {
			on = "true"
		}
		for _, f := range cur.Flags {
			if f.Name == "max_pool_size" {
				size = f.Value
			}
		}
	}
	opts := map[string]string{"mysqlConnectionPooling": "true"}
	for k, v := range show {
		opts[k] = v
	}
	return []console.Field{
		{Name: "mysqlConnectionPooling", Label: "Enable Managed Connection Pool", Type: "checkbox", Section: section, Default: on, VisibleWhen: show,
			Help: "A thread pool in the server: each client keeps a session of its own on port 3306, and its statements take turns on the pool's threads. " +
				"Enterprise Plus only. Turning it on restarts the instance, and thread_cache_size defaults to 0."},
		{Name: "mysql_pool_max_pool_size", Label: "max_pool_size", Type: "number", Section: section, Panel: "Advanced pooling options",
			VisibleWhen: opts, Default: size,
			Help: "The threads statements run on at once. Empty: one for each of the instance's vCPUs."},
	}
}

// sqlPoolConfig is the connectionPoolConfig the fields make, or nil when
// the form has none. Only the flags that differ from Google's defaults
// are sent. MySQL's fields make MySQL's when the form has them and,
// on the create page, the engine is MySQL.
func sqlPoolConfig(v map[string]string) *sqladminapi.ConnectionPoolConfig {
	if on, ok := v["mysqlConnectionPooling"]; ok && (v["engine"] == "" || v["engine"] == sqlEngineMySQL) {
		pc := &sqladminapi.ConnectionPoolConfig{ConnectionPoolingEnabled: on == "true", ForceSendFields: []string{"ConnectionPoolingEnabled"}}
		if s := strings.TrimSpace(v["mysql_pool_max_pool_size"]); pc.ConnectionPoolingEnabled && s != "" {
			pc.Flags = append(pc.Flags, &sqladminapi.ConnectionPoolFlags{Name: "max_pool_size", Value: s})
		}
		return pc
	}
	on, ok := v["connectionPooling"]
	if !ok {
		return nil
	}
	pc := &sqladminapi.ConnectionPoolConfig{ConnectionPoolingEnabled: on == "true", ForceSendFields: []string{"ConnectionPoolingEnabled"}}
	if !pc.ConnectionPoolingEnabled {
		return pc
	}
	if m := strings.TrimSpace(v["poolMode"]); m != "" && m != "transaction" {
		pc.Flags = append(pc.Flags, &sqladminapi.ConnectionPoolFlags{Name: "pool_mode", Value: m})
	}
	for _, f := range sqlPoolFlagFields {
		if s := strings.TrimSpace(v["pool_"+f.name]); s != "" && s != f.def {
			pc.Flags = append(pc.Flags, &sqladminapi.ConnectionPoolFlags{Name: f.name, Value: s})
		}
	}
	if s := strings.TrimSpace(v["pool_ignore_startup_parameters"]); s != "" {
		pc.Flags = append(pc.Flags, &sqladminapi.ConnectionPoolFlags{Name: "ignore_startup_parameters", Value: s})
	}
	return pc
}

// sqlPoolingProperty is the Overview's managed connection pooling line.
func sqlPoolingProperty(in *sqladminapi.DatabaseInstance) []console.Property {
	if in.Settings == nil || in.Settings.ConnectionPoolConfig == nil || strings.HasPrefix(in.DatabaseVersion, "SQLSERVER_") {
		return nil
	}
	pc := in.Settings.ConnectionPoolConfig
	if !pc.ConnectionPoolingEnabled {
		return []console.Property{{Label: "Managed connection pooling", Value: "Disabled"}}
	}
	if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
		size := "one thread a vCPU"
		for _, f := range pc.Flags {
			if f.Name == "max_pool_size" {
				size = "max_pool_size " + f.Value
			}
		}
		return []console.Property{{Label: "Managed connection pooling", Value: "Enabled: thread pool on port 3306, " + size}}
	}
	mode := "transaction"
	for _, f := range pc.Flags {
		if f.Name == "pool_mode" {
			mode = f.Value
		}
	}
	return []console.Property{{Label: "Managed connection pooling", Value: "Enabled: port 6432, " + mode + " mode, " +
		strconv.FormatInt(pc.PoolerCount, 10) + " pooler(s)"}}
}

// sqlPoolable says in can pool connections: Enterprise Plus PostgreSQL or
// MySQL (settings.edition, or Cloud SQL's default for PostgreSQL 16 and
// later and MySQL 8.4).
func sqlPoolable(in *sqladminapi.DatabaseInstance) bool {
	if strings.HasPrefix(in.DatabaseVersion, "SQLSERVER_") || in.Settings == nil {
		return false
	}
	switch in.Settings.Edition {
	case "ENTERPRISE_PLUS":
		return true
	case "":
		switch in.DatabaseVersion {
		case "POSTGRES_16", "POSTGRES_17", "POSTGRES_18", "MYSQL_8_4":
			return true
		}
	}
	return false
}
