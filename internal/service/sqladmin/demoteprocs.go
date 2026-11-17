package sqladmin

// demoteMaster with skipReplicationSetup (#1614): the instance becomes a
// read replica of the source representation instance's server with its
// data, as demoteMaster makes it, but its replication is not set up. Cloud
// SQL installs its replication stored procedures in the server's mysql
// schema instead, and the replication is set up by hand with them, as the
// custom import guide does after importing a dump
// (https://docs.cloud.google.com/sql/docs/mysql/replication/custom-import-replication-from-external,
// read 2026-11-17):
//
//	CALL mysql.resetMaster();
//	CALL mysql.skipTransactionWithGtid('UUID:1-N');
//	CALL mysql.setupExternalSourceAutoPosition('HOST', 3306, 'USER', 'PASSWORD', 1, false, false);
//	CALL mysql.startReplication();
//
// or, by binary log position, mysql.setupExternalSource('HOST', 3306,
// 'USER', 'PASSWORD', 'LOG_FILE', LOG_POS, false, false). The guide says
// the procedures are installed by demoteMaster and removed by
// promoteReplica; here they are too, with stopReplication.
// skipReplicationError, which the stored procedures page documents, is not
// installed: for GTID replication it commits an empty transaction for the
// failed GTID, which is not built.
//
// UNVERIFIED (docs re-read 2026-11-17, the page above): the guide gives
// the procedures' calls, not their bodies; each is the statement its name
// and parameters describe. skipTransactionWithGtid adds the GTID set it is
// given to gtid_purged (the guide calls it, after resetMaster, with the
// source's executed set); startReplication also makes the replica
// super_read_only, as a replica demoteMaster sets up is. USE_SSL sends
// SOURCE_SSL = 1, without the caCertificate check, and USE_SSL_CLIENT_AUTH
// is refused, as demoteMaster takes no client certificate here.

import (
	"context"
)

// demoteProcedures are the procedures demoteMaster with
// skipReplicationSetup installs, by name.
var demoteProcedures = []struct{ name, def string }{
	{"resetMaster", `() BEGIN
  RESET BINARY LOGS AND GTIDS;
END`},
	{"skipTransactionWithGtid", `(IN gtids TEXT) BEGIN
  SET @cloudsql_gtids = CONCAT('+', gtids);
  SET GLOBAL gtid_purged = @cloudsql_gtids;
END`},
	{"setupExternalSourceAutoPosition", `(IN host VARCHAR(255), IN port INT, IN user_name VARCHAR(255), IN user_password VARCHAR(255),
  IN master_auto_position TINYINT, IN use_ssl BOOLEAN, IN use_ssl_client_auth BOOLEAN) BEGIN
  IF use_ssl_client_auth THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'CloudBurrow does not implement SSL client authentication for external replication';
  END IF;
  SET @cloudsql_q = CONCAT('CHANGE REPLICATION SOURCE TO SOURCE_HOST = ', QUOTE(host), ', SOURCE_PORT = ', port,
    ', SOURCE_USER = ', QUOTE(user_name), ', SOURCE_PASSWORD = ', QUOTE(user_password),
    ', SOURCE_AUTO_POSITION = ', IF(master_auto_position, 1, 0), ', SOURCE_SSL = ', IF(use_ssl, 1, 0), ', GET_SOURCE_PUBLIC_KEY = 1');
  PREPARE cloudsql_stmt FROM @cloudsql_q;
  EXECUTE cloudsql_stmt;
  DEALLOCATE PREPARE cloudsql_stmt;
  SET @cloudsql_q = NULL;
END`},
	{"setupExternalSource", `(IN host VARCHAR(255), IN port INT, IN user_name VARCHAR(255), IN user_password VARCHAR(255),
  IN source_log_name VARCHAR(255), IN source_log_pos BIGINT, IN use_ssl BOOLEAN, IN use_ssl_client_auth BOOLEAN) BEGIN
  IF use_ssl_client_auth THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'CloudBurrow does not implement SSL client authentication for external replication';
  END IF;
  SET @cloudsql_q = CONCAT('CHANGE REPLICATION SOURCE TO SOURCE_HOST = ', QUOTE(host), ', SOURCE_PORT = ', port,
    ', SOURCE_USER = ', QUOTE(user_name), ', SOURCE_PASSWORD = ', QUOTE(user_password),
    ', SOURCE_AUTO_POSITION = 0, SOURCE_LOG_FILE = ', QUOTE(source_log_name), ', SOURCE_LOG_POS = ', source_log_pos,
    ', SOURCE_SSL = ', IF(use_ssl, 1, 0), ', GET_SOURCE_PUBLIC_KEY = 1');
  PREPARE cloudsql_stmt FROM @cloudsql_q;
  EXECUTE cloudsql_stmt;
  DEALLOCATE PREPARE cloudsql_stmt;
  SET @cloudsql_q = NULL;
END`},
	{"startReplication", `() BEGIN
  START REPLICA;
  SET PERSIST super_read_only = ON;
END`},
	{"stopReplication", `() BEGIN
  STOP REPLICA;
END`},
}

// installDemoteProcedures installs the procedures in the instance's
// server, replacing any left from an earlier demoteMaster, outside the
// binary log, so they give the instance no GTIDs of its own.
func installDemoteProcedures(ctx context.Context, m *MySQL) error {
	var stmts []string
	for _, p := range demoteProcedures {
		stmts = append(stmts, "DROP PROCEDURE IF EXISTS mysql."+p.name, "CREATE PROCEDURE mysql."+p.name+p.def)
	}
	return m.execUnlogged(ctx, stmts...)
}

// dropDemoteProceduresStmts are the statements promoteReplica removes the
// procedures with.
func dropDemoteProceduresStmts() []string {
	var stmts []string
	for _, p := range demoteProcedures {
		stmts = append(stmts, "DROP PROCEDURE IF EXISTS mysql."+p.name)
	}
	return stmts
}
