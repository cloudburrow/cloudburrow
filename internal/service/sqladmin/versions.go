package sqladmin

// Database versions (#1381): the databaseVersion values a created instance
// can have, and the major version upgrade, instances.patch with a newer
// databaseVersion, which Cloud SQL documents for both engines
// (docs.cloud.google.com/sql/docs/postgres/upgrade-major-db-version-inplace
// and .../mysql/upgrade-major-db-version-inplace).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// engineVersions are the served versions of each engine, oldest first:
// the order a major version upgrade goes in.
var engineVersions = map[string][]string{
	"POSTGRES": {"POSTGRES_9_6", "POSTGRES_10", "POSTGRES_11", "POSTGRES_12", "POSTGRES_13", "POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18"},
	"MYSQL":    {"MYSQL_8_0", "MYSQL_8_4"},
}

// EngineVersions are the served versions of engine, POSTGRES or MYSQL,
// oldest first.
func EngineVersions(engine string) []string { return append([]string(nil), engineVersions[engine]...) }

// versionNames are the displayName Cloud SQL gives each version.
var versionNames = map[string]string{
	"POSTGRES_9_6": "PostgreSQL 9.6", "POSTGRES_10": "PostgreSQL 10", "POSTGRES_11": "PostgreSQL 11",
	"POSTGRES_12": "PostgreSQL 12", "POSTGRES_13": "PostgreSQL 13", "POSTGRES_14": "PostgreSQL 14", "POSTGRES_15": "PostgreSQL 15",
	"POSTGRES_16": "PostgreSQL 16", "POSTGRES_17": "PostgreSQL 17", "POSTGRES_18": "PostgreSQL 18",
	"MYSQL_8_0": "MySQL 8.0", "MYSQL_8_4": "MySQL 8.4",
}

// SupportedVersions are the databaseVersion values a created instance can
// have: each is a digest-pinned official image of that major version
// (internal/components.CloudSQLVersionImages).
var SupportedVersions = func() map[string]bool {
	m := map[string]bool{}
	for _, vs := range engineVersions {
		for _, v := range vs {
			m[v] = true
		}
	}
	return m
}()

// versionIndex is version's place in its engine's order, -1 for none.
func versionIndex(version string) int {
	for i, v := range engineVersions[engineOf(version)] {
		if v == version {
			return i
		}
	}
	return -1
}

// unservedVersionWhy is why a version Cloud SQL lists is not served
// (#1462): each has no official image a linux/arm64 node can run, which
// was checked, not assumed (docker buildx imagetools inspect, 2026-10-16).
func unservedVersionWhy(version string) string {
	switch {
	case version == "MYSQL_5_7" || version == "MYSQL_5_6":
		return "MySQL " + strings.ReplaceAll(strings.TrimPrefix(version, "MYSQL_"), "_", ".") + "'s official image (mysql:5.7, last built " +
			"2023-12 at 5.7.44; mysql:5.6, 2021-12 at 5.6.51) is linux/amd64 only and no longer built, so there is no maintained " +
			"multi-architecture image to run; CloudBurrow runs " + strings.Join(engineVersions["MYSQL"], ", ")
	case strings.HasPrefix(version, "MYSQL_8_0_"):
		return "CloudBurrow runs one MySQL 8.0 minor version, MYSQL_8_0 (8.0.46, the official image's), not each " +
			"8.0 minor version Cloud SQL pins; create the instance with MYSQL_8_0"
	case strings.HasPrefix(version, "SQLSERVER_"):
		return sqlServerVersionWhy
	case strings.HasPrefix(version, "POSTGRES_"):
		return "CloudBurrow runs the PostgreSQL versions that have an official image: " + strings.Join(engineVersions["POSTGRES"], ", ")
	}
	return "CloudBurrow runs the MySQL versions " + strings.Join(engineVersions["MYSQL"], ", ")
}

// sqlServerVersionWhy is why no SQL Server version is served (#1462).
const sqlServerVersionWhy = "SQL Server has no Docker Hub official image; Microsoft's own (mcr.microsoft.com/mssql/server, " +
	"2017, 2019 and 2022) is linux/amd64 only, so a linux/arm64 node cannot run it; CloudBurrow runs PostgreSQL and MySQL"

// pgBefore12 is whether version is a PostgreSQL version before 12 (#1462),
// whose servers have no pg_promote() and no standby.signal or
// recovery.signal: their read replicas, high availability and
// point-in-time recovery are served with recovery.conf, which
// pg_basebackup -R and a recovery write, and pg_ctl promote (#1524).
func pgBefore12(version string) bool {
	return engineOf(version) == "POSTGRES" && versionIndex(version) >= 0 && versionIndex(version) < versionIndex("POSTGRES_12")
}

// upgradable is the upgradableDatabaseVersions of an instance of version:
// every newer served version of its engine.
func upgradable(version string) []any {
	i := versionIndex(version)
	if i < 0 {
		return nil
	}
	var out []any
	for _, v := range engineVersions[engineOf(version)][i+1:] {
		out = append(out, map[string]any{"majorVersion": v, "name": v, "displayName": versionNames[v]})
	}
	return out
}

// Upgrader is a Provisioner that can move an instance's server to a newer
// major version keeping its data.
type Upgrader interface {
	// Upgrade replaces from's server with one of to's databaseVersion and
	// loads from's data, users and databases into it, returning once the
	// new server serves.
	Upgrade(ctx context.Context, from, to Spec) error
}

// checkUpgrade is whether c can be upgraded to version, as Cloud SQL
// allows: to a newer major version of the same engine, on an instance
// with no read replicas that is not one.
func (s *Server) checkUpgrade(c *created, version string, includeReplicas bool) error {
	cur := c.spec.DatabaseVersion
	if legacyMySQL(cur) {
		return legacyUnimplemented("a major version upgrade")
	}
	if !SupportedVersions[version] {
		if strings.HasPrefix(version, "POSTGRES_") || strings.HasPrefix(version, "MYSQL_") {
			return unimplemented("upgrading to databaseVersion "+version, unservedVersionWhy(version))
		}
		return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for databaseVersion: %s.", version)
	}
	if engineOf(version) != engineOf(cur) {
		return Errorf(http.StatusBadRequest, "Invalid request: an instance's database engine cannot be changed (%s to %s).", cur, version)
	}
	if versionIndex(version) < versionIndex(cur) {
		return Errorf(http.StatusBadRequest, "Invalid request: downgrading the major version of an instance is not supported (%s to %s).", cur, version)
	}
	if _, ok := s.writes.prov.(Upgrader); !ok {
		return unimplemented("changing databaseVersion", "this instance's provisioner cannot replace a server")
	}
	if c.spec.MasterInstanceName != "" && c.spec.External != nil {
		// Cloud SQL: "You can't perform an in-place major version upgrade
		// on an external replica."
		return Errorf(http.StatusBadRequest, "Invalid request: an in-place major version upgrade cannot be performed on an external replica (%s).", c.spec.Name)
	}
	// MySQL replicates from a source to a replica of the next newer
	// version (MySQL 8.4, "Replication Compatibility Between MySQL
	// Versions"), so a replica is upgraded before its primary, as Cloud SQL
	// documents (#1462). A PostgreSQL replica is too (#1524), and then
	// waits for its primary (awaitPrimary).
	if engineOf(version) == "POSTGRES" {
		// A PostgreSQL replica upgraded on its own (awaitPrimary) is rebuilt
		// from its primary at the primary's version once the primary is
		// upgraded; to an older version than the replica's that would roll
		// the replica back, which Cloud SQL does not do: "After you upgrade a
		// replica, you can't roll back the database version of the replica"
		// (upgrade-major-db-version-inplace, re-read 2026-11-18). UNVERIFIED
		// (re-read 2026-11-18, which documents no error): the code and
		// wording of this refusal.
		var ahead []string
		for _, r := range s.replicaTreeLocked(c.spec.Project, c.spec.Name) {
			if rc := s.writes.byName[r]; rc != nil && versionIndex(rc.spec.DatabaseVersion) > versionIndex(version) {
				ahead = append(ahead, r+" ("+rc.spec.DatabaseVersion+")")
			}
		}
		if len(ahead) > 0 {
			return Errorf(http.StatusBadRequest, "Invalid request: the read replicas %s of %s were upgraded to a newer version than %s, and a "+
				"replica's database version cannot be rolled back; upgrade the primary to the replicas' version.",
				strings.Join(ahead, ", "), c.spec.Name, version)
		}
	}
	if rs := s.replicaNamesLocked(c.spec.Project, c.spec.Name); len(rs) > 0 && !includeReplicas {
		// Cloud SQL: "If you don't include replicas in the upgrade, then you
		// must upgrade the read replicas before you upgrade the primary
		// instance."
		var behind []string
		for _, r := range rs {
			if rc := s.writes.byName[r]; rc != nil && versionIndex(rc.spec.DatabaseVersion) < versionIndex(version) {
				behind = append(behind, r)
			}
		}
		if len(behind) > 0 {
			return Errorf(http.StatusBadRequest, "Invalid request: the read replicas of %s (%s) must be upgraded before the primary instance; "+
				"or upgrade the primary with includeReplicasForMajorVersionUpgrade set, which upgrades all its replicas after it.",
				c.spec.Name, strings.Join(behind, ", "))
		}
	}
	return nil
}

// replicaTreeLocked is every replica under name, each after its primary:
// its replicas, then theirs. Called with emu held.
func (s *Server) replicaTreeLocked(project, name string) []string {
	var out []string
	queue := []string{name}
	for len(queue) > 0 {
		rs := s.replicaNamesLocked(project, queue[0])
		queue = append(queue[1:], rs...)
		out = append(out, rs...)
	}
	return out
}

// upgradeReplicas is the second half of an upgrade with
// includeReplicasForMajorVersionUpgrade (#1421): as Cloud SQL "recreates
// the replicas", each replica under the upgraded primary, level by level,
// is rebuilt on the new version from its primary. A replica that fails is
// FAILED; the primary stays upgraded and available.
func (s *Server) upgradeReplicas(project, primary, version string) error {
	s.emu.Lock()
	names := s.replicaTreeLocked(project, primary)
	for _, n := range names {
		s.writes.byName[n].state = "MAINTENANCE"
	}
	s.emu.Unlock()
	var failed []string
	for _, n := range names {
		if err := s.upgradeReplica(n, version); err != nil {
			failed = append(failed, n+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return Errorf(http.StatusInternalServerError, "%s was upgraded to %s, and its replicas failed to: %s.", primary, version, strings.Join(failed, "; "))
	}
	return nil
}

func (s *Server) upgradeReplica(name, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	s.emu.Lock()
	c := s.writes.byName[name]
	pc := s.writes.byName[c.spec.MasterInstanceName]
	if pc == nil || pc.state != "RUNNABLE" {
		s.emu.Unlock()
		err := fmt.Errorf("its primary %s is not RUNNABLE", c.spec.MasterInstanceName)
		s.failReplica(c, err)
		return err
	}
	pe := pc.engine
	cascading := pc.spec.MasterInstanceName != ""
	spec := c.spec
	spec.DatabaseVersion = version
	spec.SettingsVersion++
	spec.ReplicationPassword = newReplicationPassword()
	spec.ReplicationStopped = false
	spec.AwaitingPrimary = false
	c.spec = spec
	c.engine = s.writes.prov.Engine(spec)
	_ = s.saveLocked()
	s.emu.Unlock()
	var err error
	if cascading {
		if cs, ok := pe.(cascader); ok {
			err = cs.PrepareCascadingSource(ctx, spec.ReplicationPassword)
		} else {
			err = fmt.Errorf("%s cannot be a cascading replication source", c.spec.MasterInstanceName)
		}
	} else if r, ok := pe.(replicator); ok {
		err = r.PrepareReplicationSource(ctx, spec.ReplicationPassword)
	} else {
		err = fmt.Errorf("%s cannot be a replication source", c.spec.MasterInstanceName)
	}
	if err == nil {
		err = s.writes.prov.Remove(ctx, spec.ServerName())
	}
	if err == nil {
		err = s.writes.prov.Start(ctx, spec)
	}
	if err != nil {
		s.failReplica(c, err)
		return err
	}
	s.emu.Lock()
	if s.writes.byName[name] == c {
		c.state, c.failure = "RUNNABLE", ""
		_ = s.saveLocked()
	}
	s.emu.Unlock()
	return nil
}

func (s *Server) failReplica(c *created, err error) {
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.writes.byName[c.spec.Name] == c {
		c.state, c.failure = "FAILED", err.Error()
		_ = s.saveLocked()
	}
}

// upgrade runs a major version upgrade of name to spec's version, as the
// operation opID.
func (s *Server) upgrade(name string, from, to Spec, opID string, includeReplicas bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	err := s.writes.prov.(Upgrader).Upgrade(ctx, from, to)
	s.emu.Lock()
	// The replicas upgraded before their primary (#1462) replicated the
	// old server, which the upgrade replaced with a new one: each is
	// rebuilt from the new server, at its own version, the primary's now.
	hasReplicas := len(s.replicaNamesLocked(to.Project, name)) > 0
	s.emu.Unlock()
	defer func() {
		if err == nil && (includeReplicas || hasReplicas) {
			err = s.upgradeReplicas(to.Project, name, to.DatabaseVersion)
		}
		s.finish(opID, err)
	}()
	s.emu.Lock()
	c := s.writes.byName[name]
	if c != nil {
		if err != nil {
			// The provisioner started the old version again with its data
			// (sqlProvisioner.Upgrade): the instance keeps its version and
			// settings, as a failed Cloud SQL upgrade rolls back.
			c.spec = from
			c.engine = s.writes.prov.Engine(c.spec)
			c.state, c.failure = "RUNNABLE", ""
		} else {
			c.state, c.failure = "RUNNABLE", ""
		}
		_ = s.saveLocked()
	}
	s.emu.Unlock()
}

// awaitPrimary upgrades the PostgreSQL read replica name on its own
// (#1524), and with include its replicas, each after its primary. Cloud
// SQL: "you must upgrade the read replicas before you upgrade the primary
// instance", and "The replicated databases of an upgraded replica are
// available only after you upgrade the primary instance." PostgreSQL's
// streaming replication does not cross major versions, so each replica's
// server is replaced by an empty one of version, AwaitingPrimary, until
// the primary's upgrade rebuilds it from the upgraded primary (upgrade,
// upgradeReplicas). A replica that fails is FAILED.
func (s *Server) awaitPrimary(project, name, version string, include bool) error {
	names := []string{name}
	if include {
		s.emu.Lock()
		names = append(names, s.replicaTreeLocked(project, name)...)
		for _, n := range names[1:] {
			s.writes.byName[n].state = "MAINTENANCE"
		}
		s.emu.Unlock()
	}
	var failed []string
	for _, n := range names {
		if err := s.awaitPrimaryOne(n, version); err != nil {
			failed = append(failed, n+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return Errorf(http.StatusInternalServerError, "the upgrade of %s to %s failed: %s.", name, version, strings.Join(failed, "; "))
	}
	return nil
}

func (s *Server) awaitPrimaryOne(name, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	s.emu.Lock()
	c := s.writes.byName[name]
	if c == nil {
		s.emu.Unlock()
		return fmt.Errorf("it was deleted")
	}
	spec := c.spec
	spec.DatabaseVersion = version
	spec.SettingsVersion++
	spec.ReplicationStopped = false
	spec.AwaitingPrimary = true
	c.spec = spec
	c.engine = s.writes.prov.Engine(spec)
	_ = s.saveLocked()
	s.emu.Unlock()
	err := s.writes.prov.Remove(ctx, spec.ServerName())
	if err == nil {
		err = s.writes.prov.Start(ctx, spec)
	}
	if err != nil {
		s.failReplica(c, err)
		return err
	}
	s.emu.Lock()
	if s.writes.byName[name] == c {
		c.state, c.failure = "RUNNABLE", ""
		_ = s.saveLocked()
	}
	s.emu.Unlock()
	return nil
}

// awaitingWhy is why a replica verb is refused on a PostgreSQL replica
// that waits for its primary's upgrade.
func awaitingWhy(spec Spec) string {
	return "the read replica " + spec.Name + " was upgraded to " + spec.DatabaseVersion + " on its own and does not replicate until its primary, " +
		spec.MasterInstanceName + ", is upgraded to " + spec.DatabaseVersion + " too; its replicated databases are available only after that, as Cloud SQL documents"
}
