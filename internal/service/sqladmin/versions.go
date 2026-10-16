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
// whose servers lack what high availability, read replicas and
// point-in-time recovery are built on here: pg_promote() and the
// recovery.signal file, both new in PostgreSQL 12.
func pgBefore12(version string) bool {
	return engineOf(version) == "POSTGRES" && versionIndex(version) >= 0 && versionIndex(version) < versionIndex("POSTGRES_12")
}

// pgBefore12Why refuses what, on an instance of version, if version is
// before PostgreSQL 12.
func pgBefore12Why(version, what string) error {
	if !pgBefore12(version) {
		return nil
	}
	return unimplemented(what+" on "+version, "CloudBurrow promotes a PostgreSQL standby with pg_promote() and recovers to a point in time "+
		"with recovery.signal, which PostgreSQL 12 introduced; "+version+" serves a primary instance's databases, users, backups, "+
		"imports, exports and major version upgrades. Upgrade it to POSTGRES_12 or later for this")
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
	if c.spec.MasterInstanceName != "" {
		if engineOf(cur) != "MYSQL" {
			return unimplemented("upgrading a PostgreSQL read replica's major version on its own",
				"a PostgreSQL replica is a physical standby streaming its primary's WAL, which PostgreSQL does not replicate across "+
					"major versions; upgrade the primary with includeReplicasForMajorVersionUpgrade, which upgrades its replicas after it, as Cloud SQL does")
		}
		// MySQL replicates from a source to a replica of the next newer
		// version (MySQL 8.4, "Replication Compatibility Between MySQL
		// Versions"), so a replica is upgraded before its primary, as
		// Cloud SQL documents (#1462).
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
		err = s.writes.prov.Remove(ctx, name)
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

// pgBefore12Unsupported refuses high availability and point-in-time
// recovery on a PostgreSQL instance before 12 (pgBefore12), when s turns
// either on: for a new instance (cur nil), or one that had it off.
func (s Spec) pgBefore12Unsupported(cur *Spec) error {
	if s.Regional() && (cur == nil || !cur.Regional()) {
		if err := pgBefore12Why(s.DatabaseVersion, "high availability (availabilityType REGIONAL)"); err != nil {
			return err
		}
	}
	if s.PITREnabled() && (cur == nil || !cur.PITREnabled()) {
		return pgBefore12Why(s.DatabaseVersion, "point-in-time recovery")
	}
	return nil
}
