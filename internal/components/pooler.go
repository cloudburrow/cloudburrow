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

// PoolerStatsUser is the pooler's console user that Cloud Monitoring's
// pooling metrics are read as (#2163).
const PoolerStatsUser = "cloudburrow_stats"

// PoolerContainer is the pooler's container and Service port name.
const PoolerContainer = "pooler"

// poolerScript writes PgBouncer's configuration and runs it. Every
// database is the server's on the pod's loopback; a client's password is
// checked against the server's own pg_shadow, read as postgres (whom the
// server trusts on its loopback), so a user signs in through the pooler
// with the password it has on the server, SCRAM or MD5. The settings
// come in CLOUDSQL_POOLER_SETTINGS, one name = value per line.
//
// PoolerStatsUser reads SHOW POOLS and SHOW STATS on the pooler's own
// pgbouncer database, for Cloud Monitoring's per_database/conn_pool
// metrics (#2163): it signs in only from the pod's loopback (the API
// server's port-forward, as the server trusts postgres there), and only to
// that database; it is no user of the server's. Every other sign-in is
// md5 against the server's password, SCRAM where the server keeps SCRAM.
const poolerScript = `set -e
d=/tmp/pgbouncer
mkdir -p "$d"
printf '"%s" "-"\n' "${CLOUDSQL_POOLER_STATS_USER}" > "$d/userlist.txt"
cat > "$d/hba.conf" <<EOF
host pgbouncer ${CLOUDSQL_POOLER_STATS_USER} 127.0.0.1/32 trust
host pgbouncer ${CLOUDSQL_POOLER_STATS_USER} ::1/128 trust
host all ${CLOUDSQL_POOLER_STATS_USER} 0.0.0.0/0 reject
host all ${CLOUDSQL_POOLER_STATS_USER} ::/0 reject
host all all 0.0.0.0/0 md5
host all all ::/0 md5
EOF
cat > "$d/pgbouncer.ini" <<EOF
[databases]
* = host=127.0.0.1 port=5432

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = ${CLOUDSQL_POOLER_PORT}
unix_socket_dir =
auth_type = hba
auth_hba_file = $d/hba.conf
auth_file = $d/userlist.txt
stats_users = ${CLOUDSQL_POOLER_STATS_USER}
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
		Env: map[string]string{"CLOUDSQL_POOLER_PORT": strconv.Itoa(port), "CLOUDSQL_POOLER_STATS_USER": PoolerStatsUser,
			"CLOUDSQL_POOLER_SETTINGS": strings.Join(lines, "\n")},
		// The image's postgres user (PgBouncer refuses to run as root).
		RunAsUser: 70}
}

// MySQLThreadPoolPort is the pod port of Cloud SQL for MySQL's thread
// pool (#2141), which the instance's Service's 3306 targets.
const MySQLThreadPoolPort = 13306

// MySQLThreadPoolStatusPort is the pod port of the thread pool's figures
// (mysqlpool's StatusHandler), which Cloud Monitoring's
// mysql/conn_pool metrics are read from through the API server's pod
// proxy (#2163); no Service publishes it.
const MySQLThreadPoolStatusPort = 13307

// MySQLThreadPoolContainer is the thread pool's container.
const MySQLThreadPoolContainer = "thread-pool"

// CloudSQLMySQLThreadPool puts Cloud SQL for MySQL's Managed Connection
// Pooling, a thread pool of maxPoolSize threads (internal/mysqlpool, run
// from image, the locally built cloudburrow-storage image), in front of
// b's mysqld on every path to it: the Service's 3306 targets the pool,
// which signs each client in to mysqld on the pod's 3306.
func CloudSQLMySQLThreadPool(b Backend, image string, maxPoolSize int) Backend {
	b.Sidecars = append(append([]Sidecar{}, b.Sidecars...), Sidecar{Name: MySQLThreadPoolContainer, Image: image, PullPolicy: "Never",
		Port: MySQLThreadPoolPort, Unpublished: true,
		Args: []string{"mysql-thread-pool", "--listen", "0.0.0.0:" + strconv.Itoa(MySQLThreadPoolPort),
			"--upstream", "127.0.0.1:" + strconv.Itoa(CloudSQLMySQLPort), "--max-pool-size", strconv.Itoa(maxPoolSize),
			"--status", "0.0.0.0:" + strconv.Itoa(MySQLThreadPoolStatusPort)}})
	b.ServiceTargetPort = MySQLThreadPoolPort
	return b
}
