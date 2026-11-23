package components

import (
	"sort"
	"strconv"
	"strings"
)

// PgBouncerImage is Cloud SQL's managed connection pooler (#2141):
// PgBouncer 1.26.1 (ISC licence, https://www.pgbouncer.org), in the
// edoburu/pgbouncer image (MIT licence, github.com/edoburu/docker-pgbouncer),
// on Alpine. Google offers no image of a pooler, so a third party's is
// pulled (PROVENANCE.md).
const PgBouncerImage = "edoburu/pgbouncer:v1.26.1-p0@sha256:488422e38cff912895d0d65d8963fcd306e6ac589a724398b6eb8e294e94d624"

// PoolerContainer is the pooler's container and Service port name.
const PoolerContainer = "pooler"

// poolerScript writes PgBouncer's configuration and runs it. Every
// database is the server's on the pod's loopback; a client's password is
// checked against the server's own pg_shadow, read as postgres (whom the
// server trusts on its loopback), so a user signs in through the pooler
// with the password it has on the server, SCRAM or MD5. The settings
// come in CLOUDSQL_POOLER_SETTINGS, one name = value per line.
const poolerScript = `set -e
d=/tmp/pgbouncer
mkdir -p "$d"
: > "$d/userlist.txt"
cat > "$d/pgbouncer.ini" <<EOF
[databases]
* = host=127.0.0.1 port=5432

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = ${CLOUDSQL_POOLER_PORT}
unix_socket_dir =
auth_type = md5
auth_file = $d/userlist.txt
auth_user = postgres
auth_dbname = postgres
auth_query = SELECT usename, passwd FROM pg_catalog.pg_shadow WHERE usename = \$1
${CLOUDSQL_POOLER_SETTINGS}
EOF
exec pgbouncer "$d/pgbouncer.ini"
`

// CloudSQLPooler is the managed connection pooler beside a PostgreSQL
// server, on port, with PgBouncer's settings by name.
func CloudSQLPooler(port int, settings map[string]string) Sidecar {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		v := settings[k]
		if k == "ignore_startup_parameters" && v == "" {
			continue
		}
		lines = append(lines, k+" = "+v)
	}
	return Sidecar{Name: PoolerContainer, Image: PgBouncerImage, Port: port,
		Command: []string{"sh", "-c", poolerScript},
		Env: map[string]string{"CLOUDSQL_POOLER_PORT": strconv.Itoa(port),
			"CLOUDSQL_POOLER_SETTINGS": strings.Join(lines, "\n")},
		// The image's postgres user (PgBouncer refuses to run as root).
		RunAsUser: 70}
}

// AlloyDBPoolerPort is AlloyDB's managed connection pooler's port (#2191):
// Google's 6432, which clients connect to directly.
const AlloyDBPoolerPort = 6432

// AlloyDBConnectorPoolerPort is the pool the AlloyDB connectors and the
// Auth Proxy are pooled in: Google keeps them a pool of their own
// (authproxy_pooler_count). Its clients have been authenticated by the
// connectors' server-side proxy (the client certificate, and the token of
// an IAM connection), as Google's is, so it checks no password: only
// that the role exists.
const AlloyDBConnectorPoolerPort = 6435

// AlloyDBPoolers are the managed connection poolers beside an AlloyDB
// instance's server: PgBouncer on AlloyDBPoolerPort, which checks each
// client's password against the server's (as Cloud SQL's does), and on
// AlloyDBConnectorPoolerPort for the connectors, with the same settings.
func AlloyDBPoolers(settings map[string]string) []Sidecar {
	direct := CloudSQLPooler(AlloyDBPoolerPort, settings)
	proxied := CloudSQLPooler(AlloyDBConnectorPoolerPort, settings)
	proxied.Name = PoolerContainer + "-connectors"
	proxied.Command = []string{"sh", "-c", strings.Replace(poolerScript, "auth_type = md5", "auth_type = trust", 1)}
	return []Sidecar{direct, proxied}
}
