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
// The long-running operations' methods, whose references name no
// permission, are not checked.
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
		}
		return ""
	},
	Methods: spannerMethods(),
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
