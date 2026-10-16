package sqladmin

// Read replicas (#1330): a replica is a created instance whose server
// replicates from its primary, by the engine's own replication: streaming
// replication from a pg_basebackup for PostgreSQL, GTID replication for
// MySQL. The replication itself is set up by the server's manifest and
// the provisioner (internal/components.CloudSQLReplicaBackend); here are
// the primary's side, promotion, and pausing.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ReplicaUser is the replication user a replica connects as.
const ReplicaUser = "cloudsqlreplica"

// replicator is an Engine that can be a replication source and a replica.
type replicator interface {
	// PrepareReplicationSource makes the server a source a replica can
	// replicate from as ReplicaUser with password.
	PrepareReplicationSource(ctx context.Context, password string) error
	// Promote makes a replica a standalone, writable primary.
	Promote(ctx context.Context) error
	// PauseReplication stops (pause) or starts applying the primary's changes.
	PauseReplication(ctx context.Context, pause bool) error
}

func newReplicationPassword() string {
	b := make([]byte, 12) // MySQL takes at most 32 characters
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// PrepareReplicationSource creates the replication role and lets it
// connect for replication; wal_level is already replica, PostgreSQL's
// default.
func (p *Postgres) PrepareReplicationSource(ctx context.Context, password string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var exists bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", ReplicaUser).Scan(&exists); err != nil {
		return pgError(err)
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	if _, err := c.Exec(ctx, verb+" ROLE "+ReplicaUser+" WITH REPLICATION LOGIN PASSWORD "+quotePGString(password)); err != nil {
		return pgError(err)
	}
	var hbaFile, hba string
	if err := c.QueryRow(ctx, "SHOW hba_file").Scan(&hbaFile); err != nil {
		return pgError(err)
	}
	if err := c.QueryRow(ctx, "SELECT pg_read_file($1)", hbaFile).Scan(&hba); err != nil {
		return pgError(err)
	}
	const line = "host replication " + ReplicaUser + " all trust"
	if !strings.Contains(hba, line) {
		// The server trusts every connection (docs/cloudsql.md); a
		// replication connection is matched only by a "replication" line.
		if _, err := c.Exec(ctx, "COPY (SELECT "+quotePGString(line)+") TO PROGRAM "+
			quotePGString("cat >> "+hbaFile)); err != nil {
			return pgError(err)
		}
		if _, err := c.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
			return pgError(err)
		}
	}
	return nil
}

// Promote ends recovery: the standby becomes a primary.
func (p *Postgres) Promote(ctx context.Context) error {
	return p.exec(ctx, "SELECT pg_promote(true, 60)")
}

// PauseReplication pauses or resumes replay of the primary's WAL.
func (p *Postgres) PauseReplication(ctx context.Context, pause bool) error {
	if pause {
		return p.exec(ctx, "SELECT pg_wal_replay_pause()")
	}
	return p.exec(ctx, "SELECT pg_wal_replay_resume()")
}

// PrepareReplicationSource turns GTIDs on, by MySQL's online procedure,
// as every Cloud SQL for MySQL instance has them, and creates the
// replication user.
func (m *MySQL) PrepareReplicationSource(ctx context.Context, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	var mode string
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_mode").Scan(&mode); err != nil {
		return myError(err)
	}
	if mode != "ON" {
		steps := []string{"SET PERSIST enforce_gtid_consistency = ON"}
		order := []string{"OFF", "OFF_PERMISSIVE", "ON_PERMISSIVE", "ON"}
		at := 0
		for i, o := range order {
			if o == mode {
				at = i
			}
		}
		for _, o := range order[at+1:] {
			steps = append(steps, "SET PERSIST gtid_mode = "+o)
		}
		for _, st := range steps {
			if strings.HasSuffix(st, "= ON") && strings.Contains(st, "gtid_mode") {
				// Every anonymous transaction must have ended first.
				for {
					var n int
					if err := db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_status "+
						"WHERE VARIABLE_NAME = 'ONGOING_ANONYMOUS_TRANSACTION_COUNT'").Scan(&n); err != nil || n == 0 {
						break
					}
					select {
					case <-ctx.Done():
						return Errorf(http.StatusConflict, "transactions without GTIDs are still running on the primary")
					case <-time.After(200 * time.Millisecond):
					}
				}
			}
			if _, err := db.ExecContext(ctx, st); err != nil {
				return myError(err)
			}
		}
	}
	for _, st := range []string{
		"CREATE USER IF NOT EXISTS '" + ReplicaUser + "'@'%' IDENTIFIED BY " + myString(password),
		"ALTER USER '" + ReplicaUser + "'@'%' IDENTIFIED BY " + myString(password),
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, RELOAD, SHOW VIEW, TRIGGER, EVENT, PROCESS ON *.* TO '" + ReplicaUser + "'@'%'",
	} {
		if _, err := db.ExecContext(ctx, st); err != nil {
			return myError(err)
		}
	}
	return nil
}

// Promote stops replication, forgets the source, and makes the server
// writable, across restarts.
func (m *MySQL) Promote(ctx context.Context) error {
	return m.exec(ctx, "STOP REPLICA", "RESET REPLICA ALL", "SET PERSIST super_read_only = OFF", "SET PERSIST read_only = OFF")
}

// PauseReplication stops or starts the replica's threads.
func (m *MySQL) PauseReplication(ctx context.Context, pause bool) error {
	if pause {
		return m.exec(ctx, "STOP REPLICA")
	}
	return m.exec(ctx, "START REPLICA")
}

// masterName is name of a masterInstanceName, which may be "project:name".
func masterName(project, v string) (string, error) {
	if p, n, ok := strings.Cut(v, ":"); ok {
		if p != project {
			return "", unimplemented("a replica of another project's instance", "cross-project replicas are not served")
		}
		return n, nil
	}
	return v, nil
}

// prepareReplica checks a replica insert against its primary and fills
// spec from it: the replica runs the primary's version, with its root.
func (s *Server) prepareReplica(ctx context.Context, project string, b instanceBody, spec *Spec) error {
	if b.FailoverReplica != nil {
		return unimplemented("failover replicas", haWhy)
	}
	if b.ReplicaConfiguration != nil {
		var rc struct {
			FailoverTarget bool            `json:"failoverTarget"`
			Mysql          json.RawMessage `json:"mysqlReplicaConfiguration"`
			Cascadable     bool            `json:"cascadableReplica"`
		}
		_ = json.Unmarshal(*b.ReplicaConfiguration, &rc)
		if rc.FailoverTarget {
			return unimplemented("failover replicas", haWhy)
		}
		if rc.Cascadable {
			return unimplemented("cascadableReplica", "it is SQL Server's setting; a MySQL or PostgreSQL replica of a replica "+
				"is created with masterInstanceName naming the replica")
		}
		if len(rc.Mysql) > 0 {
			return unimplemented("replicas of an external server", "a replica's primary is an instance of this API")
		}
	}
	if b.InstanceType != "" && b.InstanceType != "READ_REPLICA_INSTANCE" {
		return Errorf(http.StatusBadRequest, "Invalid request: an instance with masterInstanceName must have instanceType READ_REPLICA_INSTANCE.")
	}
	master, err := masterName(project, b.MasterInstanceName)
	if err != nil {
		return err
	}
	s.emu.RLock()
	pe := s.engineLocked(master)
	pc := s.writes.byName[master]
	s.emu.RUnlock()
	if pe == nil || (pc != nil && pc.spec.Project != project) {
		return Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist.")
	}
	if pc != nil && pc.state != "RUNNABLE" {
		return Errorf(http.StatusConflict, "Operation failed because the primary instance is %s.", pc.state)
	}
	// A cascading replica (#1381): its primary is itself a replica, and
	// it replicates from that replica, as Cloud SQL's do.
	cascading := pc != nil && pc.spec.MasterInstanceName != ""
	if cascading {
		if pc.spec.MasterUp {
			return unimplemented("a replica of "+master, "a replica of a replica of a server `cloudburrow up` runs is not built; "+
				"create the replicas from an instance created with instances.insert")
		}
		if d := s.replicaDepthLocked(project, master); d >= maxReplicaDepth {
			return Errorf(http.StatusBadRequest, "Invalid request: %s is already at level %d of its replication chain; "+
				"Cloud SQL allows at most %d levels of cascading replicas.", master, d, maxReplicaDepth)
		}
	}
	if spec.DatabaseVersion == "" {
		spec.DatabaseVersion = pe.DatabaseVersion()
	}
	if spec.DatabaseVersion != pe.DatabaseVersion() {
		return Errorf(http.StatusBadRequest, "Invalid request: a read replica runs its primary's databaseVersion, %s.", pe.DatabaseVersion())
	}
	r, ok := pe.(replicator)
	if !ok {
		return unimplemented("a replica of "+master, "its server cannot be a replication source")
	}
	spec.MasterInstanceName = master
	spec.MasterUp = pc == nil
	if b.Region == "" && pc != nil {
		// A replica is in its primary's region unless it is given one.
		spec.Region = pc.spec.Region
	}
	if spec.Tier == "" {
		spec.Tier = "db-custom-1-3840"
		if pc != nil {
			spec.Tier = pc.spec.Tier
		}
	}
	if m, ok := pe.(*MySQL); ok {
		// The replica's data, users included, are the primary's.
		spec.RootPassword = m.RootPassword
	}
	spec.ReplicationPassword = newReplicationPassword()
	if cascading {
		if c, ok := pe.(cascader); ok {
			err = c.PrepareCascadingSource(ctx, spec.ReplicationPassword)
		} else {
			err = unimplemented("a replica of "+master, "its server cannot be a cascading replication source")
		}
	} else {
		err = r.PrepareReplicationSource(ctx, spec.ReplicationPassword)
	}
	if err != nil {
		return fmt.Errorf("prepare %s as a replication source: %w", master, err)
	}
	return nil
}

// maxReplicaDepth is how many levels of replicas Cloud SQL allows under a
// primary: "up to four levels of replicas" (cascading replicas,
// docs.cloud.google.com/sql/docs/mysql/replication/create-replica).
const maxReplicaDepth = 4

// replicaDepthLocked is name's level in its replication chain: 0 for a
// primary, 1 for its replica, and so on. Called with emu held.
func (s *Server) replicaDepthLocked(project, name string) int {
	d := 0
	for i := 0; i < 16; i++ {
		c := s.writes.byName[name]
		if c == nil || c.spec.Project != project || c.spec.MasterInstanceName == "" {
			return d
		}
		d++
		name = c.spec.MasterInstanceName
	}
	return d
}

// cascader is an Engine that, while a replica, can be the source of a
// replica of its own.
type cascader interface {
	// PrepareCascadingSource lets a replica of this replica connect as
	// ReplicaUser with password, writing nothing to the replication
	// stream: the replica's data stays its primary's.
	PrepareCascadingSource(ctx context.Context, password string) error
}

// PrepareCascadingSource checks a standby can be a source. Its
// replication role and pg_hba.conf line are its primary's, copied by
// pg_basebackup; a standby cannot write its own, and needs none, since a
// replication connection is trusted.
func (p *Postgres) PrepareCascadingSource(ctx context.Context, _ string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var exists bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1 AND rolreplication)", ReplicaUser).Scan(&exists); err != nil {
		return pgError(err)
	}
	if !exists {
		return Errorf(http.StatusConflict, "the replica has no replication role %s yet", ReplicaUser)
	}
	return nil
}

// PrepareCascadingSource creates the replication user on a MySQL replica
// with its binary log off for the session, so the account is the
// replica's alone and adds no GTID of its own to the stream its replicas
// apply (an errant transaction, in MySQL's words). A replica logs the
// transactions it applies (log_replica_updates, on by default since 8.0),
// which is what its replicas replicate.
func (m *MySQL) PrepareCascadingSource(ctx context.Context, password string) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return myError(err)
	}
	defer conn.Close()
	for _, st := range []string{
		"SET SESSION sql_log_bin = 0",
		"SET GLOBAL super_read_only = OFF",
		"CREATE USER IF NOT EXISTS '" + ReplicaUser + "'@'%' IDENTIFIED BY " + myString(password),
		"ALTER USER '" + ReplicaUser + "'@'%' IDENTIFIED BY " + myString(password),
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, RELOAD, SHOW VIEW, TRIGGER, EVENT, PROCESS ON *.* TO '" + ReplicaUser + "'@'%'",
		"SET GLOBAL super_read_only = ON",
	} {
		if _, err := conn.ExecContext(ctx, st); err != nil {
			_, _ = conn.ExecContext(ctx, "SET GLOBAL super_read_only = ON")
			return myError(err)
		}
	}
	return nil
}

// replicaNames are the created replicas of name. Called with emu held.
func (s *Server) replicaNamesLocked(project, name string) []string {
	var out []string
	if s.writes == nil {
		return nil
	}
	for n, c := range s.writes.byName {
		if c.spec.Project == project && c.spec.MasterInstanceName == name {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// replicaVerb is promoteReplica, stopReplica and startReplica.
func (s *Server) replicaVerb(base, project, name, verb string) (any, error) {
	c, err := s.createdFor(project, name, "instances."+verb)
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	if c.spec.MasterInstanceName == "" {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "The instance is not a read replica.")
	}
	if c.state != "RUNNABLE" {
		st := c.state
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", st)
	}
	s.emu.Unlock()
	r, ok := c.engine.(replicator)
	if !ok {
		return nil, unimplemented("instances."+verb, "this server cannot be a replica")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var opType string
	switch verb {
	case "promoteReplica":
		opType = "PROMOTE_REPLICA"
		err = r.Promote(ctx)
	case "stopReplica":
		opType = "STOP_REPLICA"
		err = r.PauseReplication(ctx, true)
	case "startReplica":
		opType = "START_REPLICA"
		err = r.PauseReplication(ctx, false)
	}
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	switch verb {
	case "promoteReplica":
		// MasterUp stays: it says whose users the data has.
		c.spec.MasterInstanceName, c.spec.ReplicationPassword, c.spec.ReplicationStopped = "", "", false
	case "stopReplica":
		c.spec.ReplicationStopped = true
	case "startReplica":
		c.spec.ReplicationStopped = false
	}
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.done(base, project, c.engine, opType, name), nil
}
