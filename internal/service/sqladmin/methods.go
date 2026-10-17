// The method table is the discovery document of google.golang.org/api/sqladmin/v1
// (v0.298.0), so a method this server does not route is still listed, and
// answers 501.

package sqladmin

// Method is one method of the discovery document: its ID, HTTP method,
// path, and whether CloudBurrow serves it. Every other method answers 501.
type Method struct {
	ID, HTTPMethod, Path string
	Implemented          bool
}

// Methods is every sqladmin v1 method, from the discovery document.
var Methods = []Method{
	{"sql.Backups.CreateBackup", "POST", "v1/projects/{projectsId}/backups", true},
	{"sql.Backups.DeleteBackup", "DELETE", "v1/projects/{projectsId}/backups/{backupsId}", true},
	{"sql.Backups.GetBackup", "GET", "v1/projects/{projectsId}/backups/{backupsId}", true},
	{"sql.Backups.ListBackups", "GET", "v1/projects/{projectsId}/backups", true},
	{"sql.Backups.UpdateBackup", "PATCH", "v1/projects/{projectsId}/backups/{backupsId}", true},
	{"sql.backupRuns.delete", "DELETE", "v1/projects/{project}/instances/{instance}/backupRuns/{id}", true},
	{"sql.backupRuns.get", "GET", "v1/projects/{project}/instances/{instance}/backupRuns/{id}", true},
	{"sql.backupRuns.insert", "POST", "v1/projects/{project}/instances/{instance}/backupRuns", true},
	{"sql.backupRuns.list", "GET", "v1/projects/{project}/instances/{instance}/backupRuns", true},
	{"sql.blueGreenDeployments.create", "POST", "v1/projects/{projectsId}/locations/{locationsId}/blueGreenDeployments", false},
	{"sql.blueGreenDeployments.delete", "DELETE", "v1/projects/{projectsId}/locations/{locationsId}/blueGreenDeployments/{blueGreenDeploymentsId}", false},
	{"sql.blueGreenDeployments.get", "GET", "v1/projects/{projectsId}/locations/{locationsId}/blueGreenDeployments/{blueGreenDeploymentsId}", false},
	{"sql.blueGreenDeployments.list", "GET", "v1/projects/{projectsId}/locations/{locationsId}/blueGreenDeployments", false},
	{"sql.blueGreenDeployments.switchover", "POST", "v1/projects/{projectsId}/locations/{locationsId}/blueGreenDeployments/{blueGreenDeploymentsId}:switchover", false},
	{"sql.connect.generateEphemeral", "POST", "v1/projects/{project}/instances/{instance}:generateEphemeralCert", true},
	{"sql.connect.get", "GET", "v1/projects/{project}/instances/{instance}/connectSettings", true},
	{"sql.connect.resolve", "GET", "v1/locations/{location}/dns/{dnsName}:resolveConnectSettings", false},
	{"sql.databases.delete", "DELETE", "v1/projects/{project}/instances/{instance}/databases/{database}", true},
	{"sql.databases.get", "GET", "v1/projects/{project}/instances/{instance}/databases/{database}", true},
	{"sql.databases.insert", "POST", "v1/projects/{project}/instances/{instance}/databases", true},
	{"sql.databases.list", "GET", "v1/projects/{project}/instances/{instance}/databases", true},
	{"sql.databases.patch", "PATCH", "v1/projects/{project}/instances/{instance}/databases/{database}", true},
	{"sql.databases.update", "PUT", "v1/projects/{project}/instances/{instance}/databases/{database}", true},
	{"sql.flags.list", "GET", "v1/flags", true},
	{"sql.instances.ListEntraIdCertificates", "GET", "v1/projects/{project}/instances/{instance}/listEntraIdCertificates", false},
	{"sql.instances.ListServerCertificates", "GET", "v1/projects/{project}/instances/{instance}/listServerCertificates", false},
	{"sql.instances.RotateEntraIdCertificate", "POST", "v1/projects/{project}/instances/{instance}/rotateEntraIdCertificate", false},
	{"sql.instances.RotateServerCertificate", "POST", "v1/projects/{project}/instances/{instance}/rotateServerCertificate", false},
	{"sql.instances.acquireSsrsLease", "POST", "v1/projects/{project}/instances/{instance}/acquireSsrsLease", false},
	{"sql.instances.addEntraIdCertificate", "POST", "v1/projects/{project}/instances/{instance}/addEntraIdCertificate", false},
	{"sql.instances.addServerCa", "POST", "v1/projects/{project}/instances/{instance}/addServerCa", true},
	{"sql.instances.addServerCertificate", "POST", "v1/projects/{project}/instances/{instance}/addServerCertificate", false},
	{"sql.instances.clone", "POST", "v1/projects/{project}/instances/{instance}/clone", true},
	{"sql.instances.delete", "DELETE", "v1/projects/{project}/instances/{instance}", true},
	{"sql.instances.demote", "POST", "v1/projects/{project}/instances/{instance}/demote", true},
	{"sql.instances.demoteMaster", "POST", "v1/projects/{project}/instances/{instance}/demoteMaster", true},
	{"sql.instances.executeSql", "POST", "v1/projects/{project}/instances/{instance}/executeSql", true},
	{"sql.instances.export", "POST", "v1/projects/{project}/instances/{instance}/export", true},
	{"sql.instances.failover", "POST", "v1/projects/{project}/instances/{instance}/failover", true},
	{"sql.instances.get", "GET", "v1/projects/{project}/instances/{instance}", true},
	{"sql.instances.import", "POST", "v1/projects/{project}/instances/{instance}/import", true},
	{"sql.instances.insert", "POST", "v1/projects/{project}/instances", true},
	{"sql.instances.list", "GET", "v1/projects/{project}/instances", true},
	{"sql.instances.listServerCas", "GET", "v1/projects/{project}/instances/{instance}/listServerCas", true},
	{"sql.instances.patch", "PATCH", "v1/projects/{project}/instances/{instance}", true},
	{"sql.instances.pointInTimeRestore", "POST", "v1/projects/{projectsId}:pointInTimeRestore", true},
	{"sql.instances.preCheckMajorVersionUpgrade", "POST", "v1/projects/{project}/instances/{instance}/preCheckMajorVersionUpgrade", true},
	{"sql.instances.promoteReplica", "POST", "v1/projects/{project}/instances/{instance}/promoteReplica", true},
	{"sql.instances.reencrypt", "POST", "v1/projects/{project}/instances/{instance}/reencrypt", false},
	{"sql.instances.releaseSsrsLease", "POST", "v1/projects/{project}/instances/{instance}/releaseSsrsLease", false},
	{"sql.instances.resetSslConfig", "POST", "v1/projects/{project}/instances/{instance}/resetSslConfig", true},
	{"sql.instances.restart", "POST", "v1/projects/{project}/instances/{instance}/restart", true},
	{"sql.instances.restoreBackup", "POST", "v1/projects/{project}/instances/{instance}/restoreBackup", true},
	{"sql.instances.rotateServerCa", "POST", "v1/projects/{project}/instances/{instance}/rotateServerCa", true},
	{"sql.instances.startReplica", "POST", "v1/projects/{project}/instances/{instance}/startReplica", true},
	{"sql.instances.stopReplica", "POST", "v1/projects/{project}/instances/{instance}/stopReplica", true},
	{"sql.instances.switchover", "POST", "v1/projects/{project}/instances/{instance}/switchover", false},
	{"sql.instances.truncateLog", "POST", "v1/projects/{project}/instances/{instance}/truncateLog", true},
	{"sql.instances.update", "PUT", "v1/projects/{project}/instances/{instance}", true},
	{"sql.operations.cancel", "POST", "v1/projects/{project}/operations/{operation}/cancel", true},
	{"sql.operations.get", "GET", "v1/projects/{project}/operations/{operation}", true},
	{"sql.operations.list", "GET", "v1/projects/{project}/operations", true},
	{"sql.projects.instances.getDiskShrinkConfig", "GET", "v1/projects/{project}/instances/{instance}/getDiskShrinkConfig", true},
	{"sql.projects.instances.getLatestRecoveryTime", "GET", "v1/projects/{project}/instances/{instance}/getLatestRecoveryTime", true},
	{"sql.projects.instances.performDiskShrink", "POST", "v1/projects/{project}/instances/{instance}/performDiskShrink", true},
	{"sql.projects.instances.rescheduleMaintenance", "POST", "v1/projects/{project}/instances/{instance}/rescheduleMaintenance", false},
	{"sql.projects.instances.resetReplicaSize", "POST", "v1/projects/{project}/instances/{instance}/resetReplicaSize", true},
	{"sql.projects.instances.startExternalSync", "POST", "v1/projects/{project}/instances/{instance}/startExternalSync", true},
	{"sql.projects.instances.verifyExternalSyncSettings", "POST", "v1/projects/{project}/instances/{instance}/verifyExternalSyncSettings", true},
	{"sql.sslCerts.createEphemeral", "POST", "v1/projects/{project}/instances/{instance}/createEphemeral", true},
	{"sql.sslCerts.delete", "DELETE", "v1/projects/{project}/instances/{instance}/sslCerts/{sha1Fingerprint}", true},
	{"sql.sslCerts.get", "GET", "v1/projects/{project}/instances/{instance}/sslCerts/{sha1Fingerprint}", true},
	{"sql.sslCerts.insert", "POST", "v1/projects/{project}/instances/{instance}/sslCerts", true},
	{"sql.sslCerts.list", "GET", "v1/projects/{project}/instances/{instance}/sslCerts", true},
	{"sql.tiers.list", "GET", "v1/projects/{project}/tiers", true},
	{"sql.users.delete", "DELETE", "v1/projects/{project}/instances/{instance}/users", true},
	{"sql.users.get", "GET", "v1/projects/{project}/instances/{instance}/users/{name}", true},
	{"sql.users.insert", "POST", "v1/projects/{project}/instances/{instance}/users", true},
	{"sql.users.list", "GET", "v1/projects/{project}/instances/{instance}/users", true},
	{"sql.users.update", "PUT", "v1/projects/{project}/instances/{instance}/users", true},
}

// MethodStatuses maps each method ID to whether it is implemented.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(Methods))
	for _, m := range Methods {
		out[m.ID] = m.Implemented
	}
	return out
}
