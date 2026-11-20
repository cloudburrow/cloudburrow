package iamgate

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Firestore is Firestore's table (Native mode's data and Admin APIs):
// "Identity and Access Management (IAM)", "Required permissions for API
// methods" (https://docs.cloud.google.com/firestore/native/docs/security/iam,
// read 2026-11-20), each permission checked on the database the request
// names (resource.type firestore.googleapis.com/Database), whose project's,
// folders' and organization's policies apply: a Firestore database keeps
// no policy of its own.
// A denial is PERMISSION_DENIED "Missing or
// insufficient permissions.", Firestore's wording. The indexes take the
// table's datastore.schemas.* and the fields the same (the predefined
// roles hold no other index permission); exports and imports (datastore.databases.export,
// datastore.databases.import) and operations (datastore.operations.*) take
// the permissions of the roles the same page lists for them, as their REST
// references name none; BulkDeleteDocuments datastore.databases.bulkDelete
// (the page's Bulk Admin role) and user creds (Firestore with MongoDB
// compatibility's) datastore.userCreds.create, get, list, delete, and
// update for enable, disable and resetPassword, by the catalog's names.
//
// A commit's, a batch write's and a write stream's writes each need what
// that table gives their kind: an update or transform with
// current_document.exists false datastore.entities.create, with exists
// true (or an update_time precondition, which only an existing document
// meets) datastore.entities.update, with no precondition both; a delete
// datastore.entities.delete. A write stream's every message is checked,
// on the database its first names; Listen, which the table does not list,
// needs what a query does, on each message. ExecutePipeline needs what its
// stages do, as the table lists them (nested pipelines' too):
// collection, collection_group and database datastore.entities.get and
// list, documents datastore.entities.get, update datastore.entities.update,
// delete datastore.entities.delete.
var Firestore = &Service{
	Name: "firestore.googleapis.com",
	Type: func(resource string) string {
		if strings.Contains(resource, "/backups/") {
			return "firestore.googleapis.com/Backup"
		}
		if strings.Contains(resource, "/databases/") {
			return "firestore.googleapis.com/Database"
		}
		return ""
	},
	Methods: firestoreMethods(),
	Deny: func(string, string) error {
		return status.Error(codes.PermissionDenied, "Missing or insufficient permissions.")
	},
}

// FirestoreDatabase is the database of a Firestore resource name
// ("projects/p/databases/d/documents/c/x": "projects/p/databases/d").
func FirestoreDatabase(name string) string { return Prefix(name, 2) }

const (
	fsData  = "/google.firestore.v1.Firestore/"
	fsAdmin = "/google.firestore.admin.v1.FirestoreAdmin/"
	fsOps   = "/google.longrunning.Operations/"
)

func firestoreMethods() map[string]Rule {
	db := FirestoreDatabase
	get, list := "datastore.entities.get", "datastore.entities.list"
	m := map[string]Rule{
		fsData + "GetDocument":         On("name", db, get),
		fsData + "ListDocuments":       On("parent", db, get, list),
		fsData + "CreateDocument":      On("parent", db, "datastore.entities.create"),
		fsData + "UpdateDocument":      On("document.name", db, "datastore.entities.update"),
		fsData + "DeleteDocument":      On("name", db, "datastore.entities.delete"),
		fsData + "BatchGetDocuments":   On("database", db, get),
		fsData + "BeginTransaction":    On("database", db, "datastore.databases.get"),
		fsData + "Rollback":            On("database", db, "datastore.databases.get"),
		fsData + "Commit":              firestoreWrites,
		fsData + "BatchWrite":          firestoreWrites,
		fsData + "Write":               firestoreWrites,
		fsData + "RunQuery":            On("parent", db, get, list),
		fsData + "RunAggregationQuery": On("parent", db, get, list),
		fsData + "PartitionQuery":      On("parent", db, get, list),
		fsData + "ListCollectionIds":   On("parent", db, list),
		fsData + "Listen":              On("database", db, get, list),
		fsData + "ExecutePipeline":     firestorePipeline,

		fsAdmin + "CreateDatabase":  On("parent", nil, "datastore.databases.create"),
		fsAdmin + "GetDatabase":     On("name", nil, "datastore.databases.getMetadata"),
		fsAdmin + "ListDatabases":   On("parent", nil, "datastore.databases.list"),
		fsAdmin + "UpdateDatabase":  On("database.name", nil, "datastore.databases.update"),
		fsAdmin + "DeleteDatabase":  On("name", nil, "datastore.databases.delete"),
		fsAdmin + "RestoreDatabase": On("backup", nil, "datastore.backups.restoreDatabase"),
		fsAdmin + "CloneDatabase":   On("parent", nil, "datastore.databases.clone"),

		fsAdmin + "CreateIndex":     On("parent", db, "datastore.schemas.create"),
		fsAdmin + "GetIndex":        On("name", db, "datastore.schemas.get"),
		fsAdmin + "ListIndexes":     On("parent", db, "datastore.schemas.list"),
		fsAdmin + "DeleteIndex":     On("name", db, "datastore.schemas.delete"),
		fsAdmin + "GetField":        On("name", db, "datastore.schemas.get"),
		fsAdmin + "ListFields":      On("parent", db, "datastore.schemas.list"),
		fsAdmin + "UpdateField":     On("field.name", db, "datastore.schemas.update"),
		fsAdmin + "ExportDocuments": On("name", nil, "datastore.databases.export"),
		fsAdmin + "ImportDocuments": On("name", nil, "datastore.databases.import"),

		fsAdmin + "BulkDeleteDocuments": On("name", nil, "datastore.databases.bulkDelete"),
		fsAdmin + "CreateUserCreds":     On("parent", nil, "datastore.userCreds.create"),
		fsAdmin + "GetUserCreds":        On("name", db, "datastore.userCreds.get"),
		fsAdmin + "ListUserCreds":       On("parent", nil, "datastore.userCreds.list"),
		fsAdmin + "EnableUserCreds":     On("name", db, "datastore.userCreds.update"),
		fsAdmin + "DisableUserCreds":    On("name", db, "datastore.userCreds.update"),
		fsAdmin + "ResetUserPassword":   On("name", db, "datastore.userCreds.update"),
		fsAdmin + "DeleteUserCreds":     On("name", db, "datastore.userCreds.delete"),

		fsAdmin + "GetBackup":    On("name", nil, "datastore.backups.get"),
		fsAdmin + "ListBackups":  On("parent", nil, "datastore.backups.list"),
		fsAdmin + "DeleteBackup": On("name", nil, "datastore.backups.delete"),

		fsAdmin + "CreateBackupSchedule": On("parent", nil, "datastore.backupSchedules.create"),
		fsAdmin + "GetBackupSchedule":    On("name", db, "datastore.backupSchedules.get"),
		fsAdmin + "ListBackupSchedules":  On("parent", nil, "datastore.backupSchedules.list"),
		fsAdmin + "UpdateBackupSchedule": On("backup_schedule.name", db, "datastore.backupSchedules.update"),
		fsAdmin + "DeleteBackupSchedule": On("name", db, "datastore.backupSchedules.delete"),

		fsOps + "GetOperation":    On("name", db, "datastore.operations.get"),
		fsOps + "ListOperations":  On("name", db, "datastore.operations.list"),
		fsOps + "CancelOperation": On("name", db, "datastore.operations.cancel"),
		fsOps + "DeleteOperation": On("name", db, "datastore.operations.delete"),
	}
	return m
}

// firestoreWrites are a Commit's, BatchWrite's or Write's checks.
func firestoreWrites(m protoreflect.Message) []Check {
	db := FirestoreDatabase(String(m, "database"))
	if db == "" {
		return nil
	}
	need := map[string]bool{}
	for _, w := range List(m, "writes") {
		switch {
		case Has(w, "delete"):
			need["datastore.entities.delete"] = true
		case Has(w, "current_document.exists"):
			exists := w.Get(w.Descriptor().Fields().ByName("current_document")).Message()
			if exists.Get(exists.Descriptor().Fields().ByName("exists")).Bool() {
				need["datastore.entities.update"] = true
			} else {
				need["datastore.entities.create"] = true
			}
		case Has(w, "current_document.update_time"):
			need["datastore.entities.update"] = true
		default:
			need["datastore.entities.create"] = true
			need["datastore.entities.update"] = true
		}
	}
	var out []Check
	for _, p := range []string{"datastore.entities.create", "datastore.entities.update", "datastore.entities.delete"} {
		if need[p] {
			out = append(out, Check{p, db})
		}
	}
	return out
}

// pipelineStage is the permissions of a pipeline stage, by its name, as
// "Required permissions for API methods" lists them for executePipeline.
var pipelineStage = map[string][]string{
	"collection":       {"datastore.entities.get", "datastore.entities.list"},
	"collection_group": {"datastore.entities.get", "datastore.entities.list"},
	"database":         {"datastore.entities.get", "datastore.entities.list"},
	"documents":        {"datastore.entities.get"},
	"update":           {"datastore.entities.update"},
	"delete":           {"datastore.entities.delete"},
}

// firestorePipeline is ExecutePipeline's checks: each stage's, in every
// pipeline the request holds (subpipelines in expressions too).
func firestorePipeline(m protoreflect.Message) []Check {
	db := FirestoreDatabase(String(m, "database"))
	if db == "" {
		return nil
	}
	need := map[string]bool{}
	walkStages(m, func(name string) {
		for _, p := range pipelineStage[name] {
			need[p] = true
		}
	})
	var out []Check
	for _, p := range []string{"datastore.entities.get", "datastore.entities.list", "datastore.entities.update", "datastore.entities.delete"} {
		if need[p] {
			out = append(out, Check{p, db})
		}
	}
	return out
}

// walkStages calls f with the name of each google.firestore.v1.Pipeline.Stage
// in m, at any depth.
func walkStages(m protoreflect.Message, f func(string)) {
	if m.Descriptor().FullName() == "google.firestore.v1.Pipeline.Stage" {
		f(String(m, "name"))
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walkStages(mv.Message(), f)
					return true
				})
			}
		case fd.IsList():
			if fd.Kind() == protoreflect.MessageKind {
				for i := 0; i < v.List().Len(); i++ {
					walkStages(v.List().Get(i).Message(), f)
				}
			}
		case fd.Kind() == protoreflect.MessageKind:
			walkStages(v.Message(), f)
		}
		return true
	})
}
