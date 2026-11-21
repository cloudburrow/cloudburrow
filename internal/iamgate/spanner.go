package iamgate

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Spanner is Spanner's table: each method's REST reference
// (https://docs.cloud.google.com/spanner/docs/reference/rest/v1, read
// 2026-11-20), "Authorization requires the following IAM permission on the
// specified resource": a session's calls on the session's database
// (spanner.databases.select for ExecuteSql and ExecuteStreamingSql,
// spanner.databases.read for Read and StreamingRead,
// spanner.databases.write for Commit, ExecuteBatchDml and BatchWrite),
// BeginTransaction by its mode (spanner.databases.beginReadOnlyTransaction,
// beginOrRollbackReadWriteTransaction or beginPartitionedDmlTransaction,
// the IAM page's descriptions), the admin APIs' methods on the instance,
// database or backup they name. A database's and its instance's own
// policies apply with the project's. A denial is PERMISSION_DENIED
// "Caller is missing IAM permission P on resource R.", Spanner's wording.
// TestIamPermissions (both admin APIs') answers the requested permissions
// a caller with a token holds, by the evaluator.
// The long-running operations' methods, whose references name no
// permission, take the operation permissions of the IAM page's list by
// what the operation is of (spanner.databaseOperations.*,
// backupOperations.*, instanceOperations.*, instanceConfigOperations.*,
// instancePartitionOperations.*), on that database, backup, instance,
// instance configuration or partition: Get and Wait .get, List .list,
// Cancel .cancel, Delete .delete, or .cancel for a database's or a
// backup's operation, for which the list has no delete.
// ListDatabaseOperations, ListBackupOperations,
// ListInstanceConfigOperations and ListInstancePartitionOperations take
// the .list their references name, on their parent. AddSplitPoints
// (spanner.databases.addSplitPoints), CopyBackup (spanner.backups.create on
// the instance, spanner.backups.copy on the source), MoveInstance
// (spanner.instances.update), ListDatabaseRoles (spanner.databaseRoles.list;
// its page spells it databasesRoles, which Google's permission index does
// not have), backup schedules, instance configs and instance partitions
// take what their references name (CreateBackupSchedule says "one or more
// of" spanner.backupSchedules.create and spanner.databases.createBackup:
// the first).
var Spanner = &Service{
	Name: "spanner.googleapis.com",
	Type: func(resource string) string {
		parts := strings.Split(resource, "/")
		switch {
		case len(parts) == 6 && parts[4] == "databases":
			return "spanner.googleapis.com/Database"
		case len(parts) == 6 && parts[4] == "backups":
			return "spanner.googleapis.com/Backup"
		case len(parts) == 4 && parts[2] == "instances":
			return "spanner.googleapis.com/Instance"
		case len(parts) == 4 && parts[2] == "instanceConfigs":
			return "spanner.googleapis.com/InstanceConfig"
		case len(parts) == 6 && parts[4] == "instancePartitions":
			return "spanner.googleapis.com/InstancePartition"
		}
		return ""
	},
	Methods: spannerMethods(),
	TestIAM: map[string]bool{spDBAdmin + "TestIamPermissions": true, spInsAdmin + "TestIamPermissions": true},
	Deny: func(permission, resource string) error {
		return status.Error(codes.PermissionDenied, fmt.Sprintf("Caller is missing IAM permission %s on resource %s.", permission, resource))
	},
}

// SpannerDatabase is the database of a session's name.
func SpannerDatabase(name string) string { return Prefix(name, 3) }

const (
	spData     = "/google.spanner.v1.Spanner/"
	spDBAdmin  = "/google.spanner.admin.database.v1.DatabaseAdmin/"
	spInsAdmin = "/google.spanner.admin.instance.v1.InstanceAdmin/"
)

func spannerMethods() map[string]Rule {
	db := SpannerDatabase
	return map[string]Rule{
		spData + "CreateSession":       On("database", nil, "spanner.sessions.create"),
		spData + "BatchCreateSessions": On("database", nil, "spanner.sessions.create"),
		spData + "GetSession":          On("name", nil, "spanner.sessions.get"),
		spData + "ListSessions":        On("database", nil, "spanner.sessions.list"),
		spData + "DeleteSession":       On("name", nil, "spanner.sessions.delete"),
		spData + "ExecuteSql":          On("session", db, "spanner.databases.select"),
		spData + "ExecuteStreamingSql": On("session", db, "spanner.databases.select"),
		spData + "ExecuteBatchDml":     On("session", db, "spanner.databases.write"),
		spData + "Read":                On("session", db, "spanner.databases.read"),
		spData + "StreamingRead":       On("session", db, "spanner.databases.read"),
		spData + "BeginTransaction":    spannerBegin,
		spData + "Commit":              On("session", db, "spanner.databases.write"),
		spData + "Rollback":            On("session", db, "spanner.databases.beginOrRollbackReadWriteTransaction"),
		spData + "PartitionQuery":      On("session", db, "spanner.databases.partitionQuery"),
		spData + "PartitionRead":       On("session", db, "spanner.databases.partitionRead"),
		spData + "BatchWrite":          On("session", db, "spanner.databases.write"),

		spDBAdmin + "CreateDatabase":       On("parent", nil, "spanner.databases.create"),
		spDBAdmin + "GetDatabase":          On("name", nil, "spanner.databases.get"),
		spDBAdmin + "ListDatabases":        On("parent", nil, "spanner.databases.list"),
		spDBAdmin + "UpdateDatabase":       On("database.name", nil, "spanner.databases.update"),
		spDBAdmin + "GetDatabaseDdl":       On("database", nil, "spanner.databases.getDdl"),
		spDBAdmin + "UpdateDatabaseDdl":    On("database", nil, "spanner.databases.updateDdl"),
		spDBAdmin + "DropDatabase":         On("database", nil, "spanner.databases.drop"),
		spDBAdmin + "GetIamPolicy":         spannerIAM("getIamPolicy"),
		spDBAdmin + "SetIamPolicy":         spannerIAM("setIamPolicy"),
		spDBAdmin + "RestoreDatabase":      both(On("parent", nil, "spanner.databases.create"), On("backup", nil, "spanner.backups.restoreDatabase")),
		spDBAdmin + "CreateBackup":         both(On("parent", nil, "spanner.backups.create"), On("backup.database", nil, "spanner.databases.createBackup")),
		spDBAdmin + "GetBackup":            On("name", nil, "spanner.backups.get"),
		spDBAdmin + "ListBackups":          On("parent", nil, "spanner.backups.list"),
		spDBAdmin + "UpdateBackup":         On("backup.name", nil, "spanner.backups.update"),
		spDBAdmin + "DeleteBackup":         On("name", nil, "spanner.backups.delete"),
		spInsAdmin + "CreateInstance":      On("parent", nil, "spanner.instances.create"),
		spInsAdmin + "GetInstance":         On("name", nil, "spanner.instances.get"),
		spInsAdmin + "ListInstances":       On("parent", nil, "spanner.instances.list"),
		spInsAdmin + "UpdateInstance":      On("instance.name", nil, "spanner.instances.update"),
		spInsAdmin + "DeleteInstance":      On("name", nil, "spanner.instances.delete"),
		spInsAdmin + "GetIamPolicy":        On("resource", nil, "spanner.instances.getIamPolicy"),
		spInsAdmin + "SetIamPolicy":        On("resource", nil, "spanner.instances.setIamPolicy"),
		spInsAdmin + "GetInstanceConfig":   On("name", nil, "spanner.instanceConfigs.get"),
		spInsAdmin + "ListInstanceConfigs": On("parent", nil, "spanner.instanceConfigs.list"),

		spDBAdmin + "ListDatabaseOperations":           On("parent", nil, "spanner.databaseOperations.list"),
		spDBAdmin + "ListBackupOperations":             On("parent", nil, "spanner.backupOperations.list"),
		spInsAdmin + "ListInstanceConfigOperations":    On("parent", nil, "spanner.instanceConfigOperations.list"),
		spInsAdmin + "ListInstancePartitionOperations": On("parent", nil, "spanner.instancePartitionOperations.list"),

		spDBAdmin + "AddSplitPoints":       On("database", nil, "spanner.databases.addSplitPoints"),
		spDBAdmin + "CopyBackup":           both(On("parent", nil, "spanner.backups.create"), On("source_backup", nil, "spanner.backups.copy")),
		spDBAdmin + "ListDatabaseRoles":    On("parent", nil, "spanner.databaseRoles.list"),
		spDBAdmin + "CreateBackupSchedule": On("parent", nil, "spanner.backupSchedules.create"),
		spDBAdmin + "GetBackupSchedule":    On("name", nil, "spanner.backupSchedules.get"),
		spDBAdmin + "ListBackupSchedules":  On("parent", nil, "spanner.backupSchedules.list"),
		spDBAdmin + "UpdateBackupSchedule": On("backup_schedule.name", nil, "spanner.backupSchedules.update"),
		spDBAdmin + "DeleteBackupSchedule": On("name", nil, "spanner.backupSchedules.delete"),

		spInsAdmin + "MoveInstance":            On("name", nil, "spanner.instances.update"),
		spInsAdmin + "CreateInstanceConfig":    On("parent", nil, "spanner.instanceConfigs.create"),
		spInsAdmin + "UpdateInstanceConfig":    On("instance_config.name", nil, "spanner.instanceConfigs.update"),
		spInsAdmin + "DeleteInstanceConfig":    On("name", nil, "spanner.instanceConfigs.delete"),
		spInsAdmin + "CreateInstancePartition": On("parent", nil, "spanner.instancePartitions.create"),
		spInsAdmin + "GetInstancePartition":    On("name", nil, "spanner.instancePartitions.get"),
		spInsAdmin + "ListInstancePartitions":  On("parent", nil, "spanner.instancePartitions.list"),
		spInsAdmin + "UpdateInstancePartition": On("instance_partition.name", nil, "spanner.instancePartitions.update"),
		spInsAdmin + "DeleteInstancePartition": On("name", nil, "spanner.instancePartitions.delete"),

		spOps + "GetOperation":    spannerOperation("get"),
		spOps + "WaitOperation":   spannerOperation("get"),
		spOps + "ListOperations":  spannerOperation("list"),
		spOps + "CancelOperation": spannerOperation("cancel"),
		spOps + "DeleteOperation": spannerOperation("delete"),
	}
}

const spOps = "/google.longrunning.Operations/"

// spannerOperation is an Operations method's permission, by the resource
// the operation (or, for ListOperations, the collection) name is under.
func spannerOperation(verb string) Rule {
	return func(m protoreflect.Message) []Check {
		name := String(m, "name")
		res, _, ok := strings.Cut(name, "/operations")
		if !ok || res == "" {
			return nil
		}
		parts := strings.Split(res, "/")
		if len(parts) < 4 || len(parts)%2 != 0 {
			return nil
		}
		kind := map[string]string{"databases": "databaseOperations", "backups": "backupOperations", "instances": "instanceOperations",
			"instanceConfigs": "instanceConfigOperations", "instancePartitions": "instancePartitionOperations"}[parts[len(parts)-2]]
		if kind == "" {
			return nil
		}
		v := verb
		if v == "delete" && (kind == "databaseOperations" || kind == "backupOperations") {
			v = "cancel"
		}
		return []Check{{"spanner." + kind + "." + v, res}}
	}
}

// spannerBegin is BeginTransaction's permission, by the transaction's mode.
func spannerBegin(m protoreflect.Message) []Check {
	db := SpannerDatabase(String(m, "session"))
	if db == "" {
		return nil
	}
	switch {
	case Has(m, "options.read_only"):
		return []Check{{"spanner.databases.beginReadOnlyTransaction", db}}
	case Has(m, "options.partitioned_dml"):
		return []Check{{"spanner.databases.beginPartitionedDmlTransaction", db}}
	}
	return []Check{{"spanner.databases.beginOrRollbackReadWriteTransaction", db}}
}

// spannerIAM is DatabaseAdmin's IAM method's permission, by the
// resource's type (a database's or a backup's).
func spannerIAM(verb string) Rule {
	return func(m protoreflect.Message) []Check {
		res := String(m, "resource")
		kind := "databases"
		switch {
		case strings.Contains(res, "/backupSchedules/"):
			kind = "backupSchedules"
		case strings.Contains(res, "/backups/"):
			kind = "backups"
		case strings.Contains(res, "/databaseRoles/"):
			kind = "databaseRoles"
		}
		if res == "" {
			return nil
		}
		return []Check{{"spanner." + kind + "." + verb, res}}
	}
}

// both is the checks of each rule.
func both(rules ...Rule) Rule {
	return func(m protoreflect.Message) []Check {
		var out []Check
		for _, r := range rules {
			out = append(out, r(m)...)
		}
		return out
	}
}
