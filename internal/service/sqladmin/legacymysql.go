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
// its database flags, restart and delete, its databases and users,
// connecting through the connector, backups and their restores, clones and
// restores to a new instance, and major version upgrades, one major
// version at a time (5.6 to 5.7, 5.7 to 8.0), which are mysqldump's, as
// 8.0's are. Read replicas, high availability, point-in-time recovery,
// demotion and the external syncs are 501: CloudBurrow's replication,
// standby and recovery steps are written in MySQL 8.0's statements (CHANGE
// REPLICATION SOURCE, START REPLICA, SET PERSIST), which 5.7 and 5.6 have
// not.

import (
	"net/http"
	"regexp"
	"strings"
)

// legacyMySQL is whether version is a MySQL version before 8.0 that
// Cloud SQL still lists.
func legacyMySQL(version string) bool { return version == "MYSQL_5_7" || version == "MYSQL_5_6" }

// IsLegacyMySQL is whether version is MYSQL_5_7 or MYSQL_5_6.
func IsLegacyMySQL(version string) bool { return legacyMySQL(version) }

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

// copyRefusal is the 501 for what (a clone, a restore to a new instance)
// of an instance of version, or nil: MySQL 5.7 and 5.6 instances are
// copied by mysqldump as 8.0's are (#1524), on a node that runs their
// image; a SQL Server instance's are not built.
func (s *Server) copyRefusal(version, what string) error {
	if IsSQLServer(version) {
		return sqlServerUnimplemented(what)
	}
	if ok, why := s.versionServed(version); !ok {
		return unimplemented(what+" of a "+version+" instance", why)
	}
	return nil
}

// legacyMySQLWhy is the reason a MySQL 5.7 or 5.6 instance cannot have
// what legacyRefusal refuses.
const legacyMySQLWhy = "a physical (XtraBackup) sync restores Percona XtraBackup 8.0's or 8.4's backup, which is MySQL 8.0's and " +
	"8.4's only; a MYSQL_5_7 or MYSQL_5_6 replica of an external server is synced with mysqldump (syncMode ONLINE or OFFLINE)"

// legacyUnimplemented is the 501 for what, on a MySQL 5.7 or 5.6 instance.
func legacyUnimplemented(what string) error {
	return unimplemented(what+" on a MYSQL_5_7 or MYSQL_5_6 instance", legacyMySQLWhy)
}

// legacyRefusal is the 501 for the settings of spec a MySQL 5.7 or 5.6
// instance cannot have here, or nil. Read replicas, REGIONAL, point-in-time
// recovery, demotion and external replicas run MySQL 8.0's steps in 5.7's
// and 5.6's statements (MySQLDialect).
func legacyRefusal(spec Spec) error {
	if !legacyMySQL(spec.DatabaseVersion) {
		return nil
	}
	if ext := spec.External; ext != nil && ext.Physical {
		return legacyUnimplemented("a physical external sync")
	}
	return nil
}

// mysql56CascadeErr is Cloud SQL's refusal of a replica of a MySQL 5.6
// replica: a cascading replica's source logs what it applies, and Cloud
// SQL turns a read replica's binary logging on for "MySQL 5.7 and later
// only" (docs.cloud.google.com/sql/docs/mysql/replication/create-replica).
func mysql56CascadeErr(master string) error {
	return Errorf(http.StatusBadRequest, "Invalid request: %s is a read replica of a MYSQL_5_6 instance; cascading replicas "+
		"need binary logging on the replica, which is supported on MySQL 5.7 and later only.", master)
}

// mysql56BlueGreenErr is Cloud SQL's refusal of a blue-green deployment
// of a MySQL 5.6 instance: "your instance must run Cloud SQL for MySQL
// version 5.7, 8.0, or 8.4"
// (docs.cloud.google.com/sql/docs/mysql/create-blue-green-deployment).
func mysql56BlueGreenErr(name string) error {
	return Errorf(http.StatusBadRequest, "Invalid request: %s runs MYSQL_5_6; to create a blue-green deployment, the instance "+
		"must run Cloud SQL for MySQL version 5.7, 8.0, or 8.4.", name)
}

// MySQL 5.7's and 5.6's names for MySQL 8.0's replication statements,
// options, status fields and semi-synchronous replication variables, in
// the order MySQLDialect replaces them (#1524). The 8.0 names went in at
// 8.0.22 (START REPLICA), 8.0.23 (CHANGE REPLICATION SOURCE TO) and 8.0.26
// (rpl_semi_sync_source); the old ones are the only ones 5.7 and 5.6 have.
var legacyReplacer = strings.NewReplacer(
	"CHANGE REPLICATION SOURCE TO", "CHANGE MASTER TO",
	"START REPLICA", "START SLAVE",
	"STOP REPLICA", "STOP SLAVE",
	"RESET REPLICA", "RESET SLAVE",
	"SHOW REPLICA STATUS", "SHOW SLAVE STATUS",
	"RESET BINARY LOGS AND GTIDS", "RESET MASTER",
	"SHOW BINARY LOG STATUS", "SHOW MASTER STATUS",
	", GET_SOURCE_PUBLIC_KEY = 1", "", // caching_sha2_password's, which 5.7 has not
	"--source-data", "--master-data",
	"Replica_IO_Running", "Slave_IO_Running",
	"Replica_SQL_Running", "Slave_SQL_Running",
	"Relay_Source_Log_File", "Relay_Master_Log_File",
	"Exec_Source_Log_Pos", "Exec_Master_Log_Pos",
	"Read_Source_Log_Pos", "Read_Master_Log_Pos",
	"Seconds_Behind_Source", "Seconds_Behind_Master",
	"Source_Log_File", "Master_Log_File",
	"rpl_semi_sync_source_wait_for_replica_count", "rpl_semi_sync_master_wait_for_slave_count",
	"rpl_semi_sync_source_wait_no_replica", "rpl_semi_sync_master_wait_no_slave",
	"rpl_semi_sync_source", "rpl_semi_sync_master",
	"rpl_semi_sync_replica", "rpl_semi_sync_slave",
	"Rpl_semi_sync_source_", "Rpl_semi_sync_master_",
	"semisync_source.so", "semisync_master.so",
	"semisync_replica.so", "semisync_slave.so",
	"SET PERSIST ", "SET GLOBAL ",
)

// legacySourceOption is a CHANGE REPLICATION SOURCE TO or START REPLICA
// UNTIL option, whose 5.7 and 5.6 name starts MASTER_.
var legacySourceOption = regexp.MustCompile(`\bSOURCE_(HOST|PORT|USER|PASSWORD|AUTO_POSITION|SSL|CONNECT_RETRY|LOG_FILE|LOG_POS|DELAY|RETRY_COUNT|HEARTBEAT_PERIOD)\b`)

// mysql56Replacer is what MySQL 5.6 has not even under its old names: no
// super_read_only (5.7.8), whose closest is read_only; the server's
// status in information_schema, not performance_schema (5.7.6); and its
// semi-synchronous source waits after the commit for one replica, with
// neither rpl_semi_sync_master_wait_point nor wait_for_slave_count (5.7.2
// and 5.7.3), so those settings go. A MySQL 5.6 server here has no TLS
// certificate (5.7 made them at initialisation), so a replica connects in
// the clear.
var mysql56Replacer = strings.NewReplacer(
	"super_read_only", "read_only",
	"performance_schema.global_status", "information_schema.GLOBAL_STATUS",
	"SET GLOBAL rpl_semi_sync_master_wait_point = 'AFTER_SYNC'; ", "",
	"SET GLOBAL rpl_semi_sync_master_wait_for_slave_count = 1; ", "",
	"MASTER_SSL = 1", "MASTER_SSL = 0",
)

// MySQLDialect is text, MySQL 8.0's replication statements (in SQL or a
// shell script), as version runs them: unchanged for MySQL 8.0 and later,
// in MySQL 5.7's and 5.6's names for MYSQL_5_7 and MYSQL_5_6 (#1524). SET
// PERSIST, which 8.0 added, is SET GLOBAL there: what must outlast a
// restart on those versions is in the server's options instead (the
// GTIDs, the binary log, a replica's read_only) or set again when the
// server starts (a REGIONAL instance's semi-synchronous replication).
func MySQLDialect(version, text string) string {
	if !legacyMySQL(version) {
		return text
	}
	text = legacySourceOption.ReplaceAllString(legacyReplacer.Replace(text), "MASTER_$1")
	if version == "MYSQL_5_6" {
		text = mysql56Replacer.Replace(text)
	}
	return text
}

// mysqlUpsertUser is the statements that create user@'%' with password,
// or set the password of the one there, and grant it privs on *.*: MySQL
// 5.6 has neither CREATE USER IF NOT EXISTS nor ALTER USER ... IDENTIFIED
// BY (5.7.6), and its GRANT ... IDENTIFIED BY does both.
func mysqlUpsertUser(version, user, password, privs string) []string {
	acct := "'" + user + "'@'%'"
	if version == "MYSQL_5_6" {
		return []string{"GRANT " + privs + " ON *.* TO " + acct + " IDENTIFIED BY " + myString(password)}
	}
	return []string{
		"CREATE USER IF NOT EXISTS " + acct + " IDENTIFIED BY " + myString(password),
		"ALTER USER " + acct + " IDENTIFIED BY " + myString(password),
		"GRANT " + privs + " ON *.* TO " + acct,
	}
}
