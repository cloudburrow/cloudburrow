package sqladmin

// Database versions (#1381): the databaseVersion values a created instance
// can have, and the major version upgrade, instances.patch with a newer
// databaseVersion, which Cloud SQL documents for both engines
// (docs.cloud.google.com/sql/docs/postgres/upgrade-major-db-version-inplace
// and .../mysql/upgrade-major-db-version-inplace).

import (
	"context"
	"net/http"
	"strings"
)

// engineVersions are the served versions of each engine, oldest first:
// the order a major version upgrade goes in.
var engineVersions = map[string][]string{
	"POSTGRES": {"POSTGRES_13", "POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18"},
	"MYSQL":    {"MYSQL_8_0", "MYSQL_8_4"},
}

// EngineVersions are the served versions of engine, POSTGRES or MYSQL,
// oldest first.
func EngineVersions(engine string) []string { return append([]string(nil), engineVersions[engine]...) }

// versionNames are the displayName Cloud SQL gives each version.
var versionNames = map[string]string{
	"POSTGRES_13": "PostgreSQL 13", "POSTGRES_14": "PostgreSQL 14", "POSTGRES_15": "PostgreSQL 15",
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

// unservedVersionWhy is why a version Cloud SQL lists is not served.
func unservedVersionWhy(version string) string {
	switch {
	case version == "MYSQL_5_7":
		return "MySQL 5.7's official image was last built in December 2023 (5.7.44, linux/amd64 only) and is no longer " +
			"maintained, so there is no maintained image to run; CloudBurrow runs MYSQL_8_0 and MYSQL_8_4"
	case strings.HasPrefix(version, "POSTGRES_"):
		return "CloudBurrow runs the PostgreSQL versions whose official images are maintained: " +
			strings.Join(engineVersions["POSTGRES"], ", ")
	}
	return "CloudBurrow runs the MySQL versions " + strings.Join(engineVersions["MYSQL"], ", ")
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
func (s *Server) checkUpgrade(c *created, version string) error {
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
		return unimplemented("upgrading a read replica's major version",
			"CloudBurrow upgrades a primary with no replicas; delete the replica, upgrade the primary, and create the replica again")
	}
	if rs := s.replicaNamesLocked(c.spec.Project, c.spec.Name); len(rs) > 0 {
		return unimplemented("upgrading the major version of an instance with read replicas",
			"CloudBurrow upgrades a primary with no replicas; delete its replicas ("+strings.Join(rs, ", ")+") first")
	}
	return nil
}

// upgrade runs a major version upgrade of name to spec's version, as the
// operation opID.
func (s *Server) upgrade(name string, from, to Spec, opID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	err := s.writes.prov.(Upgrader).Upgrade(ctx, from, to)
	s.emu.Lock()
	c := s.writes.byName[name]
	if c != nil {
		if err != nil {
			// The old server's data is where it was: the instance keeps
			// its version, as a failed Cloud SQL upgrade rolls back.
			c.spec.DatabaseVersion = from.DatabaseVersion
			c.engine = s.writes.prov.Engine(c.spec)
			c.state, c.failure = "RUNNABLE", ""
		} else {
			c.state, c.failure = "RUNNABLE", ""
		}
		_ = s.saveLocked()
	}
	s.emu.Unlock()
	s.finish(opID, err)
}
