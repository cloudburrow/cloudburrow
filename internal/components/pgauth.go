package components

// Password authentication on the PostgreSQL servers (#2208).
//
// Cloud SQL and AlloyDB refuse a built-in user's wrong password on every
// path: a direct TCP connection, the connectors' server-side proxy, the
// Auth Proxy, `gcloud sql connect` and the console's Studio. So does every
// PostgreSQL server here: each start rewrites the server's pg_hba.conf so
// that every TCP sign-in is checked by password (md5, which PostgreSQL 10
// and later answer with SCRAM-SHA-256 for a password stored as SCRAM, as
// password_encryption stores it by default from 14 on, as Cloud SQL's
// flag documents). Only the server's own Unix socket, inside its pod,
// trusts: the scripts CloudBurrow runs in the server's container
// (backups, exports, imports, point-in-time recovery) use it, and no user
// path reaches it.
//
// CloudBurrow's own connections through the instance's tunnel (the Admin
// API's users, databases and the rest) sign in as a management role of
// their own, as Google's do: cloudsqladmin on Cloud SQL ("used by Cloud
// SQL to manage the instance"), alloydbadmin on AlloyDB. Its password is
// generated once per CloudBurrow instance and kept owner-only in its
// state directory, so no user knows it; users.list leaves the role out,
// and the Admin API refuses to create, change or delete it.

import "strings"

// The management roles CloudBurrow signs in to a PostgreSQL server as.
const (
	PGAdminUser      = "cloudsqladmin"
	AlloyDBAdminUser = "alloydbadmin"
)

// PostgresCredentials are a CloudBurrow instance's generated PostgreSQL
// passwords: the application user's on the server `up` runs, and the
// management role's on every PostgreSQL server (cloudsqladmin,
// alloydbadmin), the same for all of them so that a replica's copy of the
// role, or a restored copy's, signs in as its primary's.
type PostgresCredentials struct {
	Password      string `json:"password"`
	AdminPassword string `json:"adminPassword"`
}

// The environment pgAuthLib reads.
const (
	pgAdminUserEnv     = "CLOUDSQL_ADMIN_USER"
	pgAdminPasswordEnv = "CLOUDSQL_ADMIN_PASSWORD"
	pgUserNameEnv      = "CLOUDSQL_USER_NAME"
	pgUserPasswordEnv  = "CLOUDSQL_USER_PASSWORD"
)

// WithPostgresAuth gives b, a PostgreSQL server, its management role
// (admin, with adminPassword) and the password user is given when it has
// none (userPassword, "" for none: a Cloud SQL instance created with no
// rootPassword, whose postgres user then cannot sign in, as Google
// documents: "You must set root passwords before you can connect").
func WithPostgresAuth(b Backend, admin, adminPassword, user, userPassword string) Backend {
	b.Env = copyEnv(b.Env)
	b.Env[pgAdminUserEnv] = admin
	b.Env[pgAdminPasswordEnv] = adminPassword
	if user != "" && userPassword != "" {
		b.Env[pgUserNameEnv] = user
		b.Env[pgUserPasswordEnv] = userPassword
	} else {
		delete(b.Env, pgUserNameEnv)
		delete(b.Env, pgUserPasswordEnv)
	}
	// The managed connection poolers read the server's pg_shadow as the
	// management role (poolerScript).
	if len(b.Sidecars) == 0 {
		return b
	}
	sidecars := append([]Sidecar{}, b.Sidecars...)
	for i, sc := range sidecars {
		if strings.HasPrefix(sc.Name, PoolerContainer) {
			sc.Env = copyEnv(sc.Env)
			sc.Env[pgAdminUserEnv], sc.Env[pgAdminPasswordEnv] = admin, adminPassword
			sidecars[i] = sc
		}
	}
	b.Sidecars = sidecars
	return b
}

// pgAuthHeader marks a pg_hba.conf pgAuthLib has rewritten.
const pgAuthHeader = "# cloudburrow-password-auth (#2208)"

// pgAuthLib is the shell the server's container runs, as root, before the
// image's entrypoint (pgAuthPrelude), and the image's initialisation runs
// once (pgAuthInit):
//
//   - cloudsql_hba rewrites pg_hba.conf: the socket trusts, every host line
//     that trusted asks for the password (md5) instead, and the read
//     replicas' replication user is held to its password on the loopback,
//     where the instance's tunnel arrives, so only another server's pod
//     replicates without one;
//   - cloudsql_roles_sql is the management role, a superuser with the
//     instance's management password, and the user's first password.
const pgAuthLib = `cloudsql_hba() {
  f="$PGDATA/pg_hba.conf"
  [ -f "$f" ] || return 0
  if ! grep -qF '` + pgAuthHeader + `' "$f"; then
    { echo '` + pgAuthHeader + `: every TCP sign-in is checked by password; only the socket in the pod trusts'
      echo 'local all all trust'
      echo 'host replication ` + SQLReplicaUser + ` 127.0.0.1/32 md5'
      echo 'host replication ` + SQLReplicaUser + ` ::1/128 md5'
      cat "$f"; } > /tmp/cloudsql-hba.new
    cat /tmp/cloudsql-hba.new > "$f"
    rm -f /tmp/cloudsql-hba.new
  fi
  sed -i -E '/` + SQLReplicaUser + `/!s/^(host(ssl|nossl)?[[:space:]].*[[:space:]])trust[[:space:]]*$/\1md5/' "$f"
}
cloudsql_roles_sql() {
  if [ -n "${CLOUDSQL_ADMIN_USER:-}" ] && [ -n "${CLOUDSQL_ADMIN_PASSWORD:-}" ]; then
    echo "DO \$\$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = '$CLOUDSQL_ADMIN_USER') THEN CREATE ROLE $CLOUDSQL_ADMIN_USER SUPERUSER LOGIN; END IF; END\$\$;"
    echo "ALTER ROLE $CLOUDSQL_ADMIN_USER SUPERUSER LOGIN PASSWORD '$CLOUDSQL_ADMIN_PASSWORD';"
  fi
  if [ -n "${CLOUDSQL_USER_NAME:-}" ] && [ -n "${CLOUDSQL_USER_PASSWORD:-}" ]; then
    echo "DO \$\$BEGIN IF EXISTS (SELECT 1 FROM pg_catalog.pg_authid WHERE rolname = '$CLOUDSQL_USER_NAME' AND rolpassword IS NULL) THEN ALTER ROLE \"$CLOUDSQL_USER_NAME\" PASSWORD '$CLOUDSQL_USER_PASSWORD'; END IF; END\$\$;"
  fi
}
`

// pgAuthPrelude runs before the image's entrypoint, as root. A data
// directory that exists has its pg_hba.conf rewritten and, unless it is a
// standby's (whose roles are its primary's), its roles set in single-user
// mode, before the server listens. An empty one is initialised by the
// entrypoint, which then runs pgAuthInit from /docker-entrypoint-initdb.d.
const pgAuthPrelude = `cat > /tmp/cloudsql-auth.lib <<'CLOUDSQL_AUTH_EOF'
` + pgAuthLib + `CLOUDSQL_AUTH_EOF
. /tmp/cloudsql-auth.lib
mkdir -p /docker-entrypoint-initdb.d
cat > /docker-entrypoint-initdb.d/zz-cloudburrow-auth.sh <<'CLOUDSQL_AUTH_EOF'
` + pgAuthInit + `CLOUDSQL_AUTH_EOF
if [ -s "$PGDATA/PG_VERSION" ]; then
  cloudsql_hba
  if [ ! -f "$PGDATA/standby.signal" ] && [ ! -f "$PGDATA/recovery.conf" ] && [ ! -f "$PGDATA/recovery.signal" ]; then
    cloudsql_asuser=$(command -v su-exec || command -v gosu)
    cloudsql_roles_sql | "$cloudsql_asuser" postgres postgres --single -D "$PGDATA" template1 >/dev/null ||
      echo "cloudsql: the management role could not be set in single-user mode" >&2
  fi
fi
`

// pgAuthInit is sourced by the image's entrypoint once it has initialised
// a new data directory, with its temporary server on the socket.
const pgAuthInit = `. /tmp/cloudsql-auth.lib
cloudsql_roles_sql | psql -v ON_ERROR_STOP=1 --no-psqlrc -q -U "$POSTGRES_USER" -d postgres >/dev/null
cloudsql_hba
`

// pgStartCommand is a PostgreSQL server's command: pgAuthPrelude, then the
// image's entrypoint with the backend's arguments.
var pgStartCommand = []string{"sh", "-c", pgAuthPrelude + `exec docker-entrypoint.sh "$@"`, "sh"}
