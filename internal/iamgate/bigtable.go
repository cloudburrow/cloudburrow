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
// exist).", with ErrorInfo IAM_PERMISSION_DENIED.
var Bigtable = &Service{
	Name: "bigtableadmin.googleapis.com",
	Type: func(resource string) string {
		parts := strings.Split(resource, "/")
		if len(parts) < 4 {
			return ""
		}
		kinds := map[string]string{"instances": "Instance", "tables": "Table", "clusters": "Cluster", "appProfiles": "AppProfile",
			"backups": "Backup", "authorizedViews": "AuthorizedView", "materializedViews": "MaterializedView", "logicalViews": "LogicalView"}
		if k, ok := kinds[parts[len(parts)-2]]; ok {
			return "bigtableadmin.googleapis.com/" + k
		}
		return ""
	},
	Methods: bigtableMethods(),
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
	}
}
