package sqladmin

// MySQL 5.7 and 5.6 (#1524): Cloud SQL still offers MYSQL_5_7 and
// MYSQL_5_6 (in extended support). Their official images, mysql:5.7
// (5.7.44) and mysql:5.6 (5.6.51), are linux/amd64 only,
// so it is served where the cluster's node can run a linux/amd64 image:
// natively, or through the binfmt_misc emulation (Rosetta or
// qemu-x86_64) that Docker Desktop registers on an arm64 machine, which
// the provisioner checks on the node (LegacyRunner). Where it cannot, the
// version stays 501 naming why.
//
// What a MySQL 5.7 or 5.6 instance serves: instances.insert, get, list, patch of
// its database flags, restart and delete, its databases and users, and
// connecting through the connector. Read replicas, high availability,
// point-in-time recovery, clones, major version upgrades, demotion and the
// external syncs are 501: CloudBurrow's replication, standby, recovery and
// upgrade steps are written in MySQL 8.0's statements (CHANGE REPLICATION
// SOURCE, START REPLICA, SET PERSIST, SHOW CREATE USER with
// print_identified_with_as_hex), which 5.7 and 5.6 have not.

// legacyMySQL is whether version is a MySQL version before 8.0 that
// Cloud SQL still lists.
func legacyMySQL(version string) bool { return version == "MYSQL_5_7" || version == "MYSQL_5_6" }

// LegacyRunner is a Provisioner that can say whether its node runs the
// linux/amd64-only image of a MySQL version before 8.0.
type LegacyRunner interface {
	// LegacyUnrunnable is why the node cannot run version's image, or ""
	// when it can.
	LegacyUnrunnable(version string) string
}

// versionServed is whether an instance of version can be created here:
// a version of SupportedVersions, or MYSQL_5_7 or MYSQL_5_6 on a node that runs its
// image. why is the 501's reason when it cannot.
func (s *Server) versionServed(version string) (ok bool, why string) {
	if SupportedVersions[version] {
		return true, ""
	}
	if IsSQLServer(version) {
		return s.sqlServerServed(version)
	}
	if legacyMySQL(version) && s.writes != nil {
		lr, isLR := s.writes.prov.(LegacyRunner)
		if !isLR {
			return false, unservedVersionWhy(version)
		}
		if why := lr.LegacyUnrunnable(version); why != "" {
			return false, why
		}
		return true, ""
	}
	return false, unservedVersionWhy(version)
}

// legacyMySQLWhy is the reason a MySQL 5.7 instance cannot have what
// legacyRefusal refuses.
const legacyMySQLWhy = "CloudBurrow's replication, standby, recovery and upgrade steps use MySQL 8.0's statements " +
	"(CHANGE REPLICATION SOURCE, START REPLICA, SET PERSIST), which MySQL 5.7 and 5.6 have not; a MYSQL_5_7 or MYSQL_5_6 instance here serves " +
	"its server, databases, users, flags and connections"

// legacyUnimplemented is the 501 for what, on a MySQL 5.7 or 5.6 instance.
func legacyUnimplemented(what string) error {
	return unimplemented(what+" on a MYSQL_5_7 or MYSQL_5_6 instance", legacyMySQLWhy)
}

// legacyRefusal is the 501 for the settings of spec a MySQL 5.7 instance
// cannot have here, or nil.
func legacyRefusal(spec Spec) error {
	if !legacyMySQL(spec.DatabaseVersion) {
		return nil
	}
	switch {
	case spec.MasterInstanceName != "":
		return legacyUnimplemented("a read replica")
	case spec.Regional():
		return legacyUnimplemented("availabilityType REGIONAL")
	case spec.PITREnabled():
		return legacyUnimplemented("binaryLogEnabled (point-in-time recovery)")
	case spec.External != nil:
		return legacyUnimplemented("an external primary's replica")
	}
	return nil
}
