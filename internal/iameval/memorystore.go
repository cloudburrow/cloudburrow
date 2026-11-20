package iameval

import "fmt"

// newerDeny is the wording of Google's newer APIs' denial, "Permission
// 'P' denied on resource 'R' (or it may not exist).", with ErrorInfo
// IAM_PERMISSION_DENIED.
func newerDeny(permission, resource string) error {
	return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
		permission, resource, true)
}

// Redis is Memorystore for Redis' table. Its REST reference pages name no
// permission, so each method's is the one "Memorystore for Redis access
// control" maps it to (https://docs.cloud.google.com/memorystore/docs/redis/access-control,
// read 2026-11-20), and GetInstanceAuthString's and FailoverInstance's
// the permissions of their names in "Memorystore for Redis roles and
// permissions" (https://docs.cloud.google.com/iam/docs/roles-permissions/redis).
// The Locations and Operations mixins on Memorystore's one port serve
// three APIs whose names they cannot tell apart, and are not checked.
// Memorystore keeps no resource-level policies. Google's wording of a
// denial is UNVERIFIED (re-read 2026-11-20: the access-control pages give
// none): newerDeny's.
var Redis = &Service{
	Name:  "redis.googleapis.com",
	Types: map[string]string{"instances": "redis.googleapis.com/Instance"},
	Methods: sm(map[string][2]string{
		"ListInstances":         {"redis.instances.list", "parent"},
		"GetInstance":           {"redis.instances.get", "name"},
		"GetInstanceAuthString": {"redis.instances.getAuthString", "name"},
		"CreateInstance":        {"redis.instances.create", "parent"},
		"UpdateInstance":        {"redis.instances.update", "instance.name"},
		"UpgradeInstance":       {"redis.instances.upgrade", "name"},
		"ImportInstance":        {"redis.instances.import", "name"},
		"ExportInstance":        {"redis.instances.export", "name"},
		"FailoverInstance":      {"redis.instances.failover", "name"},
		"DeleteInstance":        {"redis.instances.delete", "name"},
		"RescheduleMaintenance": {"redis.instances.rescheduleMaintenance", "name"},
	}, "/google.cloud.redis.v1.CloudRedis/"),
	Deny: newerDeny,
}

// RedisCluster is Memorystore for Redis Cluster's table. Its REST
// reference pages name no permission, so each method's is the one
// "Memorystore for Redis Cluster access control" maps it to
// (https://docs.cloud.google.com/memorystore/docs/cluster/access-control,
// read 2026-11-20: GetClusterCertificateAuthority is "Download the CA",
// redis.clusters.get), or the permission of its name in the roles page
// (https://docs.cloud.google.com/iam/docs/roles-permissions/redis): delete,
// backup, backup collections and backups, ACL policies. Token auth users,
// auth tokens, ACL policy revisions and the shared regional CA have no
// permission of their name there, and are not checked. Wording as Redis'.
var RedisCluster = &Service{
	Name: "redis.googleapis.com",
	Types: map[string]string{"clusters": "redis.googleapis.com/Cluster", "backupCollections": "redis.googleapis.com/BackupCollection",
		"backups": "redis.googleapis.com/Backup", "aclPolicies": "redis.googleapis.com/AclPolicy"},
	Methods: sm(map[string][2]string{
		"ListClusters":                   {"redis.clusters.list", "parent"},
		"GetCluster":                     {"redis.clusters.get", "name"},
		"CreateCluster":                  {"redis.clusters.create", "parent"},
		"UpdateCluster":                  {"redis.clusters.update", "cluster.name"},
		"DeleteCluster":                  {"redis.clusters.delete", "name"},
		"GetClusterCertificateAuthority": {"redis.clusters.get", "name"},
		"RescheduleClusterMaintenance":   {"redis.clusters.rescheduleMaintenance", "name"},
		"BackupCluster":                  {"redis.clusters.backup", "name"},
		"ListBackupCollections":          {"redis.backupCollections.list", "parent"},
		"GetBackupCollection":            {"redis.backupCollections.get", "name"},
		"ListBackups":                    {"redis.backups.list", "parent"},
		"GetBackup":                      {"redis.backups.get", "name"},
		"DeleteBackup":                   {"redis.backups.delete", "name"},
		"ExportBackup":                   {"redis.backups.export", "name"},
		"ListAclPolicies":                {"redis.aclPolicies.list", "parent"},
		"GetAclPolicy":                   {"redis.aclPolicies.get", "name"},
		"CreateAclPolicy":                {"redis.aclPolicies.create", "parent"},
		"UpdateAclPolicy":                {"redis.aclPolicies.update", "acl_policy.name"},
		"DeleteAclPolicy":                {"redis.aclPolicies.delete", "name"},
	}, "/google.cloud.redis.cluster.v1.CloudRedisCluster/"),
	Deny: newerDeny,
}

// Valkey is Memorystore for Valkey's table: the permission each method's
// REST reference names (https://docs.cloud.google.com/memorystore/docs/valkey/reference/rest/v1,
// read 2026-11-20; backups.list's page names memorystore.backups.get, as
// is checked), and, where a page names none, the permission of its name
// in "Memorystore roles and permissions"
// (https://docs.cloud.google.com/iam/docs/roles-permissions/memorystore):
// token auth users and auth tokens. The shared regional CA has none, and
// rescheduleMaintenance's (memorystore.instances.rescheduleMaintenance, of
// the access-control page) is not in Google's permission index, so
// neither is checked. Wording as Redis'.
var Valkey = &Service{
	Name: "memorystore.googleapis.com",
	Types: map[string]string{"instances": "memorystore.googleapis.com/Instance",
		"backupCollections": "memorystore.googleapis.com/BackupCollection", "backups": "memorystore.googleapis.com/Backup",
		"aclPolicies": "memorystore.googleapis.com/AclPolicy"},
	Methods: sm(map[string][2]string{
		"ListInstances":           {"memorystore.instances.list", "parent"},
		"GetInstance":             {"memorystore.instances.get", "name"},
		"CreateInstance":          {"memorystore.instances.create", "parent"},
		"UpdateInstance":          {"memorystore.instances.update", "instance.name"},
		"DeleteInstance":          {"memorystore.instances.delete", "name"},
		"GetCertificateAuthority": {"memorystore.instances.get", "name"},
		"BackupInstance":          {"memorystore.instances.backup", "name"},
		"StartMigration":          {"memorystore.instances.startMigration", "name"},
		"FinishMigration":         {"memorystore.instances.finishMigration", "name"},
		"ListBackupCollections":   {"memorystore.backupCollections.list", "parent"},
		"GetBackupCollection":     {"memorystore.backupCollections.get", "name"},
		"ListBackups":             {"memorystore.backups.get", "parent"},
		"GetBackup":               {"memorystore.backups.get", "name"},
		"DeleteBackup":            {"memorystore.backups.delete", "name"},
		"ExportBackup":            {"memorystore.backups.export", "name"},
		"AddTokenAuthUser":        {"memorystore.tokenAuthUsers.create", "instance"},
		"GetTokenAuthUser":        {"memorystore.tokenAuthUsers.get", "name"},
		"ListTokenAuthUsers":      {"memorystore.tokenAuthUsers.list", "parent"},
		"DeleteTokenAuthUser":     {"memorystore.tokenAuthUsers.delete", "name"},
		"AddAuthToken":            {"memorystore.authTokens.create", "token_auth_user"},
		"GetAuthToken":            {"memorystore.authTokens.get", "name"},
		"ListAuthTokens":          {"memorystore.authTokens.list", "parent"},
		"DeleteAuthToken":         {"memorystore.authTokens.delete", "name"},
		"CreateAclPolicy":         {"memorystore.aclPolicies.create", "parent"},
		"GetAclPolicy":            {"memorystore.aclPolicies.get", "name"},
		"ListAclPolicies":         {"memorystore.aclPolicies.list", "parent"},
		"UpdateAclPolicy":         {"memorystore.aclPolicies.update", "acl_policy.name"},
		"DeleteAclPolicy":         {"memorystore.aclPolicies.delete", "name"},
		"GetAclPolicyRevision":    {"memorystore.aclPolicyRevisions.get", "name"},
		"ListAclPolicyRevisions":  {"memorystore.aclPolicyRevisions.list", "parent"},
	}, "/google.cloud.memorystore.v1.Memorystore/"),
	Deny: newerDeny,
}
