package iamgate

import (
	"fmt"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Bigtable is Bigtable's table: the data API's RPC reference
// (https://docs.cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2)
// and the admin API's REST reference
// (https://docs.cloud.google.com/bigtable/docs/reference/admin/rest/v2),
// read 2026-11-20, "Authorization requires the following IAM permission on
// the specified resource", with "Access control with IAM"
// (https://docs.cloud.google.com/bigtable/docs/access-control): a data call
// on the table, authorized view or materialized view it names
// (bigtable.tables.readRows, .sampleRowKeys, .mutateRows, and the
// authorizedViews' and materializedViews' own), CheckAndMutateRow and
// ReadModifyWriteRow both readRows and mutateRows as the access-control page
// says, ExecuteQuery and PrepareQuery bigtable.instances.executeQuery on
// the instance; the admin methods on the instance, cluster, app profile,
// table, authorized view or backup they name. Where a reference names no
// permission (PartialUpdateInstance, PartialUpdateCluster, the IAM
// methods), the method takes the one of its name in the access-control
// page's list (bigtable.instances.update, bigtable.clusters.update,
// bigtable.tables.getIamPolicy, ...). A table's, view's or backup's own
// policy and its instance's apply with the project's. A denial is
// PERMISSION_DENIED "Permission 'P' denied on resource 'R' (or it may not
// exist).", with ErrorInfo IAM_PERMISSION_DENIED. TestIamPermissions (both
// admin APIs') answers the requested permissions a caller with a token
// holds, by the evaluator.
//
// Change streams (ReadChangeStream, GenerateInitialChangeStreamPartitions)
// need bigtable.tables.readRows on the table, PingAndWarm
// bigtable.instances.ping, as the data reference names; CheckConsistency,
// GenerateConsistencyToken, UndeleteTable, CopyBackup (backups.create on
// the parent, backups.read on the source), schema bundles
// (bigtable.schemaBundles.*), logical and materialized views
// (bigtable.logicalViews.*, bigtable.materializedViews.*) what the admin
// reference names; memory layers bigtable.memoryLayers.get, list and
// update as it names them, and ListHotTablets bigtable.hotTablets.list (the
// reference's bigtable.clusters.listHotTablets is not in Google's
// permission index, so no role could grant it; the index's, which Bigtable
// Reader holds).
//
// Snapshots, which Google calls a private alpha and whose permissions no
// reference or role names, are checked as the backups they resemble, on
// their cluster: SnapshotTable bigtable.tables.readRows on the table and
// bigtable.backups.create on the cluster, GetSnapshot, ListSnapshots and
// DeleteSnapshot bigtable.backups.get, .list and .delete, and
// CreateTableFromSnapshot bigtable.tables.create on the instance and
// bigtable.backups.restore on the snapshot's cluster.
//
// The long-running operations' methods, which name no permission (and
// Bigtable has no operations permission), take the permission of the
// resource the operation is of ("operations/projects/p/instances/i/..."):
// GetOperation, WaitOperation and ListOperations its .get, CancelOperation
// and DeleteOperation its .update (a backup's .update, a table's .update).
var Bigtable = &Service{
	Name: "bigtableadmin.googleapis.com",
	Type: func(resource string) string {
		parts := strings.Split(resource, "/")
		if len(parts) < 4 {
			return ""
		}
		kinds := map[string]string{"instances": "Instance", "tables": "Table", "clusters": "Cluster", "appProfiles": "AppProfile",
			"backups": "Backup", "authorizedViews": "AuthorizedView", "materializedViews": "MaterializedView", "logicalViews": "LogicalView",
			"schemaBundles": "SchemaBundle"}
		if k, ok := kinds[parts[len(parts)-2]]; ok {
			return "bigtableadmin.googleapis.com/" + k
		}
		return ""
	},
	Methods: bigtableMethods(),
	TestIAM: map[string]bool{btTables + "TestIamPermissions": true, btInstance + "TestIamPermissions": true},
	Deny: func(permission, resource string) error {
		st := status.New(codes.PermissionDenied, fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource))
		if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: "IAM_PERMISSION_DENIED", Domain: "iam.googleapis.com",
			Metadata: map[string]string{"permission": permission, "resource": resource}}); err == nil {
			st = d
		}
		return st.Err()
	},
}

const (
	btData     = "/google.bigtable.v2.Bigtable/"
	btTables   = "/google.bigtable.admin.v2.BigtableTableAdmin/"
	btInstance = "/google.bigtable.admin.v2.BigtableInstanceAdmin/"
)

// btDataCall is a data call's checks: verbs on the table, the authorized
// view or the materialized view the request names.
func btDataCall(verbs ...string) Rule {
	return func(m protoreflect.Message) []Check {
		var res, kind string
		switch {
		case String(m, "authorized_view_name") != "":
			res, kind = String(m, "authorized_view_name"), "authorizedViews"
		case String(m, "materialized_view_name") != "":
			res, kind = String(m, "materialized_view_name"), "materializedViews"
		default:
			res, kind = String(m, "table_name"), "tables"
		}
		if res == "" {
			return nil
		}
		out := make([]Check, len(verbs))
		for i, v := range verbs {
			out[i] = Check{"bigtable." + kind + "." + v, res}
		}
		return out
	}
}

// btIAM is an IAM method's permission, by the resource's collection.
func btIAM(verb string) Rule {
	return func(m protoreflect.Message) []Check {
		res := String(m, "resource")
		parts := strings.Split(res, "/")
		if len(parts) < 4 {
			return nil
		}
		return []Check{{"bigtable." + parts[len(parts)-2] + "." + verb, res}}
	}
}

func bigtableMethods() map[string]Rule {
	return map[string]Rule{
		btData + "ReadRows":           btDataCall("readRows"),
		btData + "SampleRowKeys":      btDataCall("sampleRowKeys"),
		btData + "MutateRow":          btDataCall("mutateRows"),
		btData + "MutateRows":         btDataCall("mutateRows"),
		btData + "CheckAndMutateRow":  btDataCall("readRows", "mutateRows"),
		btData + "ReadModifyWriteRow": btDataCall("readRows", "mutateRows"),
		btData + "ExecuteQuery":       On("instance_name", nil, "bigtable.instances.executeQuery"),
		btData + "PrepareQuery":       On("instance_name", nil, "bigtable.instances.executeQuery"),

		btInstance + "CreateInstance":        On("parent", nil, "bigtable.instances.create"),
		btInstance + "GetInstance":           On("name", nil, "bigtable.instances.get"),
		btInstance + "ListInstances":         On("parent", nil, "bigtable.instances.list"),
		btInstance + "UpdateInstance":        On("name", nil, "bigtable.instances.update"),
		btInstance + "PartialUpdateInstance": On("instance.name", nil, "bigtable.instances.update"),
		btInstance + "DeleteInstance":        On("name", nil, "bigtable.instances.delete"),
		btInstance + "GetIamPolicy":          btIAM("getIamPolicy"),
		btInstance + "SetIamPolicy":          btIAM("setIamPolicy"),
		btInstance + "CreateCluster":         On("parent", nil, "bigtable.clusters.create"),
		btInstance + "GetCluster":            On("name", nil, "bigtable.clusters.get"),
		btInstance + "ListClusters":          On("parent", nil, "bigtable.clusters.list"),
		btInstance + "UpdateCluster":         On("name", nil, "bigtable.clusters.update"),
		btInstance + "PartialUpdateCluster":  On("cluster.name", nil, "bigtable.clusters.update"),
		btInstance + "DeleteCluster":         On("name", nil, "bigtable.clusters.delete"),
		btInstance + "CreateAppProfile":      On("parent", nil, "bigtable.appProfiles.create"),
		btInstance + "GetAppProfile":         On("name", nil, "bigtable.appProfiles.get"),
		btInstance + "ListAppProfiles":       On("parent", nil, "bigtable.appProfiles.list"),
		btInstance + "UpdateAppProfile":      On("app_profile.name", nil, "bigtable.appProfiles.update"),
		btInstance + "DeleteAppProfile":      On("name", nil, "bigtable.appProfiles.delete"),

		btTables + "CreateTable":          On("parent", nil, "bigtable.tables.create"),
		btTables + "GetTable":             On("name", nil, "bigtable.tables.get"),
		btTables + "ListTables":           On("parent", nil, "bigtable.tables.list"),
		btTables + "UpdateTable":          On("table.name", nil, "bigtable.tables.update"),
		btTables + "DeleteTable":          On("name", nil, "bigtable.tables.delete"),
		btTables + "ModifyColumnFamilies": On("name", nil, "bigtable.tables.update"),
		btTables + "DropRowRange":         On("name", nil, "bigtable.tables.mutateRows"),
		btTables + "GetIamPolicy":         btIAM("getIamPolicy"),
		btTables + "SetIamPolicy":         btIAM("setIamPolicy"),
		btTables + "CreateBackup":         both(On("parent", nil, "bigtable.backups.create"), On("backup.source_table", nil, "bigtable.tables.readRows")),
		btTables + "GetBackup":            On("name", nil, "bigtable.backups.get"),
		btTables + "ListBackups":          On("parent", nil, "bigtable.backups.list"),
		btTables + "UpdateBackup":         On("backup.name", nil, "bigtable.backups.update"),
		btTables + "DeleteBackup":         On("name", nil, "bigtable.backups.delete"),
		btTables + "RestoreTable":         both(On("parent", nil, "bigtable.tables.create"), On("backup", nil, "bigtable.backups.restore")),
		btTables + "CreateAuthorizedView": On("parent", nil, "bigtable.authorizedViews.create"),
		btTables + "GetAuthorizedView":    On("name", nil, "bigtable.authorizedViews.get"),
		btTables + "ListAuthorizedViews":  On("parent", nil, "bigtable.authorizedViews.list"),
		btTables + "UpdateAuthorizedView": On("authorized_view.name", nil, "bigtable.authorizedViews.update"),
		btTables + "DeleteAuthorizedView": On("name", nil, "bigtable.authorizedViews.delete"),

		btData + "ReadChangeStream":                      btDataCall("readRows"),
		btData + "GenerateInitialChangeStreamPartitions": btDataCall("readRows"),
		btData + "PingAndWarm":                           On("name", nil, "bigtable.instances.ping"),

		btTables + "CheckConsistency":         On("name", nil, "bigtable.tables.checkConsistency"),
		btTables + "GenerateConsistencyToken": On("name", nil, "bigtable.tables.generateConsistencyToken"),
		btTables + "UndeleteTable":            On("name", nil, "bigtable.tables.undelete"),
		btTables + "CopyBackup":               both(On("parent", nil, "bigtable.backups.create"), On("source_backup", nil, "bigtable.backups.read")),
		btTables + "CreateSchemaBundle":       On("parent", nil, "bigtable.schemaBundles.create"),
		btTables + "GetSchemaBundle":          On("name", nil, "bigtable.schemaBundles.get"),
		btTables + "ListSchemaBundles":        On("parent", nil, "bigtable.schemaBundles.list"),
		btTables + "UpdateSchemaBundle":       On("schema_bundle.name", nil, "bigtable.schemaBundles.update"),
		btTables + "DeleteSchemaBundle":       On("name", nil, "bigtable.schemaBundles.delete"),

		btTables + "SnapshotTable":           both(On("name", nil, "bigtable.tables.readRows"), On("cluster", nil, "bigtable.backups.create")),
		btTables + "GetSnapshot":             On("name", cluster, "bigtable.backups.get"),
		btTables + "ListSnapshots":           On("parent", cluster, "bigtable.backups.list"),
		btTables + "DeleteSnapshot":          On("name", cluster, "bigtable.backups.delete"),
		btTables + "CreateTableFromSnapshot": both(On("parent", nil, "bigtable.tables.create"), On("source_snapshot", cluster, "bigtable.backups.restore")),

		btInstance + "CreateLogicalView":      On("parent", nil, "bigtable.logicalViews.create"),
		btInstance + "GetLogicalView":         On("name", nil, "bigtable.logicalViews.get"),
		btInstance + "ListLogicalViews":       On("parent", nil, "bigtable.logicalViews.list"),
		btInstance + "UpdateLogicalView":      On("logical_view.name", nil, "bigtable.logicalViews.update"),
		btInstance + "DeleteLogicalView":      On("name", nil, "bigtable.logicalViews.delete"),
		btInstance + "CreateMaterializedView": On("parent", nil, "bigtable.materializedViews.create"),
		btInstance + "GetMaterializedView":    On("name", nil, "bigtable.materializedViews.get"),
		btInstance + "ListMaterializedViews":  On("parent", nil, "bigtable.materializedViews.list"),
		btInstance + "UpdateMaterializedView": On("materialized_view.name", nil, "bigtable.materializedViews.update"),
		btInstance + "DeleteMaterializedView": On("name", nil, "bigtable.materializedViews.delete"),

		btInstance + "ListHotTablets":    On("parent", nil, "bigtable.hotTablets.list"),
		btInstance + "GetMemoryLayer":    On("name", nil, "bigtable.memoryLayers.get"),
		btInstance + "ListMemoryLayers":  On("parent", nil, "bigtable.memoryLayers.list"),
		btInstance + "UpdateMemoryLayer": On("memory_layer.name", nil, "bigtable.memoryLayers.update"),

		btOps + "GetOperation":    btOperation("get"),
		btOps + "WaitOperation":   btOperation("get"),
		btOps + "ListOperations":  btOperation("get"),
		btOps + "CancelOperation": btOperation("update"),
		btOps + "DeleteOperation": btOperation("update"),
	}
}

const btOps = "/google.longrunning.Operations/"

// cluster is the cluster of a snapshot's name ("projects/p/instances/i/
// clusters/c/snapshots/s"), or a cluster's own name.
func cluster(name string) string { return Prefix(name, 3) }

// btOperation is an Operations method's permission: verb on the resource
// the operation is of, by its collection.
func btOperation(verb string) Rule {
	return func(m protoreflect.Message) []Check {
		res := strings.TrimPrefix(String(m, "name"), "operations/")
		if i := strings.Index(res, "/operations/"); i >= 0 {
			res = res[:i]
		}
		parts := strings.Split(res, "/")
		if len(parts) < 4 || len(parts)%2 != 0 || parts[0] != "projects" {
			return nil
		}
		kind := parts[len(parts)-2]
		switch kind {
		case "instances", "clusters", "tables", "appProfiles", "backups", "authorizedViews", "schemaBundles",
			"logicalViews", "materializedViews":
		default:
			return nil
		}
		return []Check{{"bigtable." + kind + "." + verb, res}}
	}
}
