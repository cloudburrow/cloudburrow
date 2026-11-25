package iameval

import (
	"fmt"
	"strings"
)

// DataMigration is Database Migration Service's table (#2252): the
// permission each method's REST reference names
// (https://docs.cloud.google.com/database-migration/docs/reference/rest/v1,
// read 2026-11-25), checked on the resource its request names: the
// parent for create and list, the location for fetchStaticIps. Connection
// profiles, migration jobs and private connections keep their own
// policies; getIamPolicy and setIamPolicy take the permission of the
// resource's collection. The permissions are spelled as Google's roles
// spell them (datamigration.connectionprofiles.get). Methods CloudBurrow
// answers UNIMPLEMENTED (conversion workspaces, generateSshScript,
// generateTcpProxyScript) are not checked. Google's wording of a denial
// is UNVERIFIED: its newer APIs' "Permission 'P' denied on resource 'R'
// (or it may not exist).".
var DataMigration = &Service{
	Name:              "datamigration.googleapis.com",
	PolicyCollections: []string{"connectionProfiles", "migrationJobs", "privateConnections"},
	Types: map[string]string{"connectionProfiles": "datamigration.googleapis.com/ConnectionProfile",
		"migrationJobs":      "datamigration.googleapis.com/MigrationJob",
		"privateConnections": "datamigration.googleapis.com/PrivateConnection"},
	Methods: func() map[string]Method {
		const p = "/google.cloud.clouddms.v1.DataMigrationService/"
		m := sm(map[string][2]string{
			"ListConnectionProfiles":  {"datamigration.connectionprofiles.list", "parent"},
			"GetConnectionProfile":    {"datamigration.connectionprofiles.get", "name"},
			"CreateConnectionProfile": {"datamigration.connectionprofiles.create", "parent"},
			"UpdateConnectionProfile": {"datamigration.connectionprofiles.update", "connection_profile.name"},
			"DeleteConnectionProfile": {"datamigration.connectionprofiles.delete", "name"},
			"ListMigrationJobs":       {"datamigration.migrationjobs.list", "parent"},
			"GetMigrationJob":         {"datamigration.migrationjobs.get", "name"},
			"CreateMigrationJob":      {"datamigration.migrationjobs.create", "parent"},
			"UpdateMigrationJob":      {"datamigration.migrationjobs.update", "migration_job.name"},
			"DeleteMigrationJob":      {"datamigration.migrationjobs.delete", "name"},
			"StartMigrationJob":       {"datamigration.migrationjobs.start", "name"},
			"StopMigrationJob":        {"datamigration.migrationjobs.stop", "name"},
			"ResumeMigrationJob":      {"datamigration.migrationjobs.resume", "name"},
			"PromoteMigrationJob":     {"datamigration.migrationjobs.promote", "name"},
			"VerifyMigrationJob":      {"datamigration.migrationjobs.verify", "name"},
			"RestartMigrationJob":     {"datamigration.migrationjobs.restart", "name"},
			"CreatePrivateConnection": {"datamigration.privateconnections.create", "parent"},
			"GetPrivateConnection":    {"datamigration.privateconnections.get", "name"},
			"ListPrivateConnections":  {"datamigration.privateconnections.list", "parent"},
			"DeletePrivateConnection": {"datamigration.privateconnections.delete", "name"},
			"FetchStaticIps":          {"datamigration.locations.fetchStaticIps", "name"},
		}, p)
		for prefix, rows := range map[string]map[string][2]string{
			"/google.cloud.location.Locations/": {
				"GetLocation":   {"datamigration.locations.get", "name"},
				"ListLocations": {"datamigration.locations.list", "name"},
			},
			"/google.longrunning.Operations/": {
				"GetOperation":    {"datamigration.operations.get", "name"},
				"ListOperations":  {"datamigration.operations.list", "name"},
				"DeleteOperation": {"datamigration.operations.delete", "name"},
				"CancelOperation": {"datamigration.operations.cancel", "name"},
			},
		} {
			for k, v := range sm(rows, prefix) {
				m[k] = v
			}
		}
		for _, verb := range []string{"GetIamPolicy", "SetIamPolicy"} {
			verb := verb
			perm := "getIamPolicy"
			if verb == "SetIamPolicy" {
				perm = "setIamPolicy"
			}
			m["/google.iam.v1.IAMPolicy/"+verb] = Method{Field: "resource", PermissionFor: func(resource string) string {
				return "datamigration." + dmsCollection(resource) + "." + perm
			}}
		}
		return m
	}(),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
}

// dmsCollection is a resource name's collection as the permissions spell
// it: connectionprofiles, migrationjobs, privateconnections.
func dmsCollection(resource string) string {
	parts := strings.Split(resource, "/")
	if len(parts) >= 6 {
		return strings.ToLower(parts[4])
	}
	return "migrationjobs"
}
