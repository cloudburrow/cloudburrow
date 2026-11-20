package iameval

import (
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CloudSQL is the Cloud SQL Admin API's table, served over JSON alone
// (CloudSQLJSON). "All permissions are applied to the project. You cannot
// apply different permissions based on the instance or other lower-level
// object" ("Cloud SQL IAM permissions",
// https://docs.cloud.google.com/sql/docs/mysql/iam-permissions, read
// 2026-11-20), so Cloud SQL keeps no resource-level policies, and an
// instance's methods are checked on the instance's name for conditions'
// resource.name, by its project's policies. Google's 403 is "The client
// isn't authorized to make this request.", reason notAuthorized ("Admin
// API error messages",
// https://docs.cloud.google.com/sql/docs/mysql/admin-api-error-messages,
// read 2026-11-20), which cmd/cloudburrow writes in the API's error
// envelope.
var CloudSQL = &Service{
	Name:  "sqladmin.googleapis.com",
	Types: map[string]string{"instances": "sqladmin.googleapis.com/Instance"},
	Deny: func(permission, resource string) error {
		return status.Error(codes.PermissionDenied, CloudSQLDenied)
	},
}

// CloudSQLDenied is Cloud SQL's 403 message.
const CloudSQLDenied = "The client isn't authorized to make this request."

// CloudSQLJSON are the Cloud SQL Admin API's methods, under /v1/ and
// /sql/v1beta4/, with the permission the "Permissions required for Cloud
// SQL Admin API methods" table names
// (https://docs.cloud.google.com/sql/docs/mysql/iam-permissions, read
// 2026-11-20: patch, clone, export and import also name
// cloudsql.instances.get or create, the first named is checked), and,
// where it lists none, the permission of the method's name in "Cloud SQL
// roles and permissions" (https://docs.cloud.google.com/iam/docs/roles-permissions/cloudsql):
// the server CA and certificate methods, demoteMaster, reencrypt,
// users.get, the Entra ID certificates, and disk shrink (its feature
// page's); startExternalSync and verifyExternalSyncSettings are
// cloudsql.instances.migrate ("on the Cloud SQL replica"); the backups
// collection is the table's backupRuns permissions; operations.get and
// list cloudsql.instances.get. connectSettings and generateEphemeralCert,
// what a connector calls to connect, check cloudsql.instances.connect, the
// permission "Connect using the Cloud SQL Auth Proxy" says a 403
// notAuthorized lacks (UNVERIFIED, re-read 2026-11-20, that these methods
// check it). Not checked, with no permission documented: flags.list and
// tiers.list (the table: "None"), pointInTimeRestore, switchover, demote,
// operations.cancel, getLatestRecoveryTime, rescheduleMaintenance,
// createEphemeral, the SSRS leases and resolveConnectSettings.
var CloudSQLJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/connectSettings$`), Permission: "cloudsql.instances.connect"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+):generateEphemeralCert$`), Permission: "cloudsql.instances.connect"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/:]+)/instances$`), Permission: "cloudsql.instances.list"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/:]+)/instances$`), Permission: "cloudsql.instances.create"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)$`), Permission: "cloudsql.instances.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)$`), Permission: "cloudsql.instances.update"},
	{Method: "PUT", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)$`), Permission: "cloudsql.instances.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)$`), Permission: "cloudsql.instances.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/clone$`), Permission: "cloudsql.instances.clone"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/export$`), Permission: "cloudsql.instances.export"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/import$`), Permission: "cloudsql.instances.import"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/failover$`), Permission: "cloudsql.instances.failover"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/promoteReplica$`), Permission: "cloudsql.instances.promoteReplica"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/resetSslConfig$`), Permission: "cloudsql.instances.resetSslConfig"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/restart$`), Permission: "cloudsql.instances.restart"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/restoreBackup$`), Permission: "cloudsql.instances.restoreBackup"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/startReplica$`), Permission: "cloudsql.instances.startReplica"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/stopReplica$`), Permission: "cloudsql.instances.stopReplica"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/truncateLog$`), Permission: "cloudsql.instances.truncateLog"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/addServerCa$`), Permission: "cloudsql.instances.addServerCa"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/addServerCertificate$`), Permission: "cloudsql.instances.addServerCertificate"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/rotateServerCa$`), Permission: "cloudsql.instances.rotateServerCa"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/rotateServerCertificate$`), Permission: "cloudsql.instances.rotateServerCertificate"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/demoteMaster$`), Permission: "cloudsql.instances.demoteMaster"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/reencrypt$`), Permission: "cloudsql.instances.reencrypt"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/performDiskShrink$`), Permission: "cloudsql.instances.performDiskShrink"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/startExternalSync$`), Permission: "cloudsql.instances.migrate"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/verifyExternalSyncSettings$`), Permission: "cloudsql.instances.migrate"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/executeSql$`), Permission: "cloudsql.instances.executeSql"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/addEntraIdCertificate$`), Permission: "cloudsql.instances.addEntraIdCertificate"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/rotateEntraIdCertificate$`), Permission: "cloudsql.instances.rotateEntraIdCertificate"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/listServerCas$`), Permission: "cloudsql.instances.listServerCas"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/listServerCertificates$`), Permission: "cloudsql.instances.listServerCertificates"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/listEntraIdCertificates$`), Permission: "cloudsql.instances.listEntraIdCertificates"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/getDiskShrinkConfig$`), Permission: "cloudsql.instances.getDiskShrinkConfig"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases$`), Permission: "cloudsql.databases.list"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases$`), Permission: "cloudsql.databases.create"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases/[^/]+$`), Permission: "cloudsql.databases.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases/[^/]+$`), Permission: "cloudsql.databases.delete"},
	{Method: "PATCH", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases/[^/]+$`), Permission: "cloudsql.databases.update"},
	{Method: "PUT", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/databases/[^/]+$`), Permission: "cloudsql.databases.update"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/sslCerts$`), Permission: "cloudsql.sslCerts.list"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/sslCerts$`), Permission: "cloudsql.sslCerts.create"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/sslCerts/[^/]+$`), Permission: "cloudsql.sslCerts.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/sslCerts/[^/]+$`), Permission: "cloudsql.sslCerts.delete"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/backupRuns$`), Permission: "cloudsql.backupRuns.list"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/backupRuns$`), Permission: "cloudsql.backupRuns.create"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/backupRuns/[^/]+$`), Permission: "cloudsql.backupRuns.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/backupRuns/[^/]+$`), Permission: "cloudsql.backupRuns.delete"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/users$`), Permission: "cloudsql.users.list"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/users/[^/]+$`), Permission: "cloudsql.users.get"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/users$`), Permission: "cloudsql.users.create"},
	{Method: "PUT", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/users$`), Permission: "cloudsql.users.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+/instances/[^/:]+)/users$`), Permission: "cloudsql.users.delete"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/:]+)/backups$`), Permission: "cloudsql.backupRuns.list"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/:]+)/backups$`), Permission: "cloudsql.backupRuns.create"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+)/backups/[^/]+$`), Permission: "cloudsql.backupRuns.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+)/backups/[^/]+$`), Permission: "cloudsql.backupRuns.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+)/backups/[^/]+$`), Permission: "cloudsql.backupRuns.delete"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/]+)/operations/[^/]+$`), Permission: "cloudsql.instances.get"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/(?:v1|sql/v1beta4)/(projects/[^/:]+)/operations$`), Permission: "cloudsql.instances.get"},
}
