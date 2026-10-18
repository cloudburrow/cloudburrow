package sqladmin

// Disaster recovery replicas and instances.switchover (#1614), as Cloud SQL
// documents them for an Enterprise Plus MySQL primary
// (docs.cloud.google.com/sql/docs/mysql/use-advanced-disaster-recovery):
//
//   - a primary's replicationCluster.failoverDrReplicaName designates one
//     of its read replicas, in another region, its DR replica; the replica
//     then reports replicationCluster.drReplica;
//   - instances.switchover, called on the DR replica, reverses the roles
//     with no data lost: the primary stops taking writes (read-only), the
//     replica applies every transaction the primary has, and is promoted;
//     the old primary then replicates from it by GTID auto-positioning,
//     keeping its data, as demoteMaster's replica does, and is designated
//     the new primary's DR replica.
//
// Both servers are changed online, through their tunnels: no server is
// restarted. The old primary's other replicas go on replicating from it,
// now cascading replicas of the new primary.
//
// A PostgreSQL primary's DR replica and switchover are 501 (pgSwitchoverWhy).

import (
	"context"
	"database/sql"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"
)

// pgSwitchoverWhy is why a PostgreSQL replication cluster is not served.
const pgSwitchoverWhy = "the old primary would have to become a replica of the new one, and a PostgreSQL server becomes a standby " +
	"of another only by a rebuild of its data directory (pg_rewind or a new base backup), which is not built (demote of a PostgreSQL instance " +
	"is 501 for the same reason); a MySQL primary's DR replica and switchover are served, and a PostgreSQL replica is promoted with promoteReplica " +
	"(docs/cloudsql.md)"

// drReplicateFlags are the flags a DR replica must not have set
// ("None of these flags set to non-default values").
var drReplicateFlags = map[string]bool{"replicate_do_db": true, "replicate_ignore_db": true, "replicate_do_table": true,
	"replicate_wild_do_table": true, "replicate_ignore_table": true, "replicate_wild_ignore_table": true}

// editionOf is spec's edition: settings.edition, or Cloud SQL's default,
// Enterprise Plus for MySQL 8.4 and Enterprise for the others.
func editionOf(spec Spec) string {
	if ed, _ := spec.Settings["edition"].(string); ed != "" {
		return ed
	}
	if spec.DatabaseVersion == "MYSQL_8_4" {
		return "ENTERPRISE_PLUS"
	}
	return "ENTERPRISE"
}

// drDesignation is the DR replica a patch of primary leaves it with: its
// replicationCluster.failoverDrReplicaName checked against Cloud SQL's
// requirements for a DR replica, or its current one when the patch does
// not send it. next is the primary as patched. Called with emu held.
func (s *Server) drDesignationLocked(b instanceBody, project string, next Spec) (string, error) {
	if b.clusterCleared {
		return "", nil
	}
	if b.ReplicationCluster == nil || b.ReplicationCluster.FailoverDrReplicaName == nil {
		return next.FailoverDR, nil
	}
	v := *b.ReplicationCluster.FailoverDrReplicaName
	if v == "" {
		return "", nil // the designation removed
	}
	name := next.Name
	switch {
	case !next.MySQL():
		return "", unimplemented("a DR replica (replicationCluster.failoverDrReplicaName) of a PostgreSQL instance", pgSwitchoverWhy)
	case next.MasterInstanceName != "":
		return "", Errorf(http.StatusBadRequest, "Invalid request: %s is a read replica; replicationCluster.failoverDrReplicaName "+
			"is set on a primary instance, naming one of its replicas.", name)
	case editionOf(next) != "ENTERPRISE_PLUS":
		return "", Errorf(http.StatusBadRequest, "Invalid request: a DR replica is an Enterprise Plus edition feature; %s is %s.", name, editionOf(next))
	case next.Regional():
		return "", unimplemented("a DR replica of a high-availability (REGIONAL) instance",
			"a switchover makes the primary a read replica, and a read replica here is one server; designate the DR replica of a ZONAL primary")
	}
	dr := v
	if p, n, ok := strings.Cut(v, ":"); ok {
		if p != project {
			return "", Errorf(http.StatusBadRequest, "Invalid request: the DR replica %s must be in the primary's project, %s.", v, project)
		}
		dr = n
	}
	rc := s.writes.byName[dr]
	if rc == nil || rc.spec.Project != project || rc.spec.MasterInstanceName != name {
		return "", Errorf(http.StatusBadRequest, "Invalid request: %s is not a read replica of %s; a DR replica is one of the primary's "+
			"direct read replicas (not a cascading one).", v, name)
	}
	r := rc.spec
	switch {
	case r.External != nil:
		return "", Errorf(http.StatusBadRequest, "Invalid request: %s is an external replica, which cannot be a DR replica.", dr)
	case editionOf(r) != "ENTERPRISE_PLUS":
		return "", Errorf(http.StatusBadRequest, "Invalid request: the DR replica %s must be Enterprise Plus edition; it is %s.", dr, editionOf(r))
	case r.region() == next.region():
		return "", Errorf(http.StatusBadRequest, "Invalid request: the DR replica %s must be in a different region from its primary (%s).", dr, next.region())
	case rc.state != "RUNNABLE":
		return "", Errorf(http.StatusConflict, "Operation failed because the replica %s is %s.", dr, rc.state)
	}
	for _, f := range r.Flags {
		if drReplicateFlags[strings.ReplaceAll(f.Name, "-", "_")] {
			return "", Errorf(http.StatusBadRequest, "Invalid request: the DR replica %s has the flag %s set; a DR replica has none of %s.",
				dr, f.Name, "replicate_do_db, replicate_ignore_db, replicate_do_table, replicate_wild_do_table, replicate_ignore_table and replicate_wild_ignore_table")
		}
	}
	return dr, nil
}

// checkDRReplicaPatch refuses what a designated DR replica cannot be
// patched to: binary logging off, or a replicate_* flag. Called with emu
// held.
func (s *Server) checkDRReplicaPatchLocked(next Spec, raw map[string]any) error {
	if !s.isDRReplicaLocked(next) {
		return nil
	}
	if bc, ok := raw["backupConfiguration"].(map[string]any); ok {
		if on, ok := bc["binaryLogEnabled"].(bool); ok && !on {
			return Errorf(http.StatusBadRequest, "Invalid request: you can't turn off binary logging while %s is a designated DR replica.", next.Name)
		}
	}
	for _, f := range next.Flags {
		n := strings.ReplaceAll(f.Name, "-", "_")
		if drReplicateFlags[n] || n == "skip_log_bin" || n == "disable_log_bin" {
			return Errorf(http.StatusBadRequest, "Invalid request: %s is a designated DR replica, which cannot have the flag %s.", next.Name, f.Name)
		}
	}
	return nil
}

// isDRReplicaLocked is whether spec is its primary's DR replica.
func (s *Server) isDRReplicaLocked(spec Spec) bool {
	if spec.MasterInstanceName == "" || s.writes == nil {
		return false
	}
	p := s.writes.byName[spec.MasterInstanceName]
	return p != nil && p.spec.Project == spec.Project && p.spec.FailoverDR == spec.Name
}

// dropDRLocked removes name's designation as its primary's DR replica,
// once it is deleted or promoted.
func (s *Server) dropDRLocked(spec Spec) {
	if s.isDRReplicaLocked(spec) {
		s.writes.byName[spec.MasterInstanceName].spec.FailoverDR = ""
	}
}

// replicationClusterJSON adds replicationCluster to a created instance's
// JSON: the primary's failoverDrReplicaName, or a DR replica's drReplica.
func (s *Server) replicationClusterJSON(in map[string]any, spec Spec) {
	s.emu.RLock()
	defer s.emu.RUnlock()
	switch {
	case spec.FailoverDR != "":
		if r := s.writes.byName[spec.FailoverDR]; r != nil && r.spec.MasterInstanceName == spec.Name {
			in["replicationCluster"] = map[string]any{"failoverDrReplicaName": spec.Project + ":" + spec.FailoverDR}
		}
	case s.isDRReplicaLocked(spec):
		in["replicationCluster"] = map[string]any{"drReplica": true}
	}
}

// switchover is instances.switchover, called on the DR replica name.
func (s *Server) switchover(r *http.Request, base, project, name string) (any, error) {
	if r.Method != http.MethodPost {
		return nil, Errorf(http.StatusNotFound, "method %s is not served on switchover", r.Method)
	}
	timeout := 10 * time.Minute
	if v := r.URL.Query().Get("dbTimeout"); v != "" {
		d, err := parseSeconds(v)
		if err != nil {
			return nil, err
		}
		if d <= 0 || d > 24*time.Hour {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: dbTimeout %s is not between 0 and 24 hours.", v)
		}
		timeout = d
	}
	c, err := s.createdFor(project, name, "instances.switchover")
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	spec := c.spec
	var pc *created
	if spec.MasterInstanceName != "" {
		pc = s.writes.byName[spec.MasterInstanceName]
	}
	fail := func(err error) (any, error) {
		s.emu.Unlock()
		return nil, err
	}
	switch {
	case !spec.MySQL():
		return fail(unimplemented("instances.switchover of a PostgreSQL instance", pgSwitchoverWhy))
	case spec.MasterInstanceName == "":
		return fail(Errorf(http.StatusBadRequest, "Invalid request: %s is not a read replica; switchover is called on the DR replica "+
			"of a primary instance.", name))
	case pc == nil || pc.spec.Project != project || pc.spec.FailoverDR != name:
		return fail(Errorf(http.StatusBadRequest, "Invalid request: %s is not the DR replica of its primary %s: designate it with "+
			"the primary's replicationCluster.failoverDrReplicaName first.", name, spec.MasterInstanceName))
	case c.state != "RUNNABLE":
		return fail(Errorf(http.StatusConflict, "Operation failed because the DR replica %s is %s.", name, c.state))
	case pc.state != "RUNNABLE":
		return fail(Errorf(http.StatusConflict, "Operation failed because the primary instance %s is %s.", pc.spec.Name, pc.state))
	case spec.ReplicationStopped:
		return fail(Errorf(http.StatusBadRequest, "Invalid request: replication to %s is stopped; start it with startReplica first.", name))
	case pc.spec.Regional():
		return fail(unimplemented("a switchover from a high-availability (REGIONAL) primary",
			"the primary becomes a read replica, and a read replica here is one server"))
	}
	c.state, pc.state = "MAINTENANCE", "MAINTENANCE"
	s.emu.Unlock()
	op := s.pending(base, project, name, "SWITCHOVER")
	go func() {
		err := s.runSwitchover(c, pc, timeout)
		s.finish(op["name"].(string), err)
	}()
	return op, nil
}

// runSwitchover reverses the roles of the primary pc and its DR replica c.
func (s *Server) runSwitchover(c, pc *created, timeout time.Duration) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	newPrimary, ok1 := c.engine.(*MySQL)
	oldPrimary, ok2 := pc.engine.(*MySQL)
	if !ok1 || !ok2 {
		s.emu.Lock()
		c.state, pc.state = "RUNNABLE", "RUNNABLE"
		s.emu.Unlock()
		return unimplemented("instances.switchover", "these servers are not MySQL servers")
	}
	s.emu.RLock()
	name, master := c.spec.Name, pc.spec.Name
	s.emu.RUnlock()
	promoted := false
	defer func() {
		if err == nil {
			return
		}
		if !promoted {
			// The primary takes writes again; nothing else changed.
			_ = oldPrimary.exec(context.Background(), "SET PERSIST super_read_only = OFF", "SET PERSIST read_only = OFF")
		}
		s.emu.Lock()
		c.state, pc.state = "RUNNABLE", "RUNNABLE"
		s.emu.Unlock()
	}()
	// The primary stops accepting writes and becomes read-only.
	if err := oldPrimary.exec(ctx, "SET PERSIST read_only = ON", "SET PERSIST super_read_only = ON"); err != nil {
		return fmt.Errorf("make %s read-only: %w", master, err)
	}
	gtids, err := queryString(ctx, oldPrimary, "SELECT @@GLOBAL.gtid_executed")
	if err != nil {
		return fmt.Errorf("read %s's GTIDs: %w", master, err)
	}
	// The replica applies every transaction the primary has: no data lost.
	if err := waitForGTIDs(ctx, newPrimary, gtids, timeout); err != nil {
		return fmt.Errorf("the DR replica %s did not apply %s's transactions within dbTimeout %s: %w", name, master, timeout, err)
	}
	if err := newPrimary.Promote(ctx); err != nil {
		return fmt.Errorf("promote %s: %w", name, err)
	}
	promoted = true
	password := newReplicationPassword()
	if err := newPrimary.PrepareReplicationSource(ctx, password); err != nil {
		return fmt.Errorf("make %s a replication source: %w", name, err)
	}
	// The old primary replicates from the new one by GTID auto-positioning,
	// keeping its data, with a server ID of its own as a replica's.
	serverID := 2 + crc32.ChecksumIEEE([]byte(master))%1000000
	if err := oldPrimary.exec(ctx,
		fmt.Sprintf("SET PERSIST server_id = %d", serverID),
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST = "+myString(instanceServicePrefix+name)+", SOURCE_PORT = 3306, SOURCE_USER = "+
			myString(ReplicaUser)+", SOURCE_PASSWORD = "+myString(password)+", SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1",
		"START REPLICA"); err != nil {
		return fmt.Errorf("make %s a replica of %s: %w", master, name, err)
	}
	s.emu.Lock()
	ns, olds := c.spec, pc.spec
	ns.MasterInstanceName, ns.MasterUp, ns.ReplicationPassword, ns.ReplicationStopped, ns.Seeded = "", false, "", false, false
	ns.FailoverDR = master // the old primary is the new one's DR replica
	olds.MasterInstanceName, olds.MasterUp, olds.ReplicationPassword, olds.ReplicationStopped, olds.Seeded = name, false, password, false, true
	olds.FailoverDR, olds.External, olds.ExternalSyncStarted = "", nil, false
	olds.BackupEnabled = false // a replica's automated backups are off
	ns.SettingsVersion++
	olds.SettingsVersion++
	c.spec, pc.spec = ns, olds
	c.state, pc.state = "RUNNABLE", "RUNNABLE"
	err = s.saveLocked()
	s.emu.Unlock()
	return err
}

func queryString(ctx context.Context, m *MySQL, q string) (string, error) {
	db, err := m.db()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return "", myError(err)
	}
	return v.String, nil
}

// waitForGTIDs waits for m to have executed gtids.
func waitForGTIDs(ctx context.Context, m *MySQL, gtids string, timeout time.Duration) error {
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	var r sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT WAIT_FOR_EXECUTED_GTID_SET(?, ?)", strings.ReplaceAll(gtids, "\n", ""),
		int64(timeout/time.Second)).Scan(&r); err != nil {
		return myError(err)
	}
	if !r.Valid || r.Int64 != 0 {
		return fmt.Errorf("timed out")
	}
	return nil
}
