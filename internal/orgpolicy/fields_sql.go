package orgpolicy

// Cloud SQL's custom constraint resource types and their documented
// fields ("Add custom organization policies", Cloud SQL for MySQL,
// supported resources, read 2026-11-19,
// https://docs.cloud.google.com/sql/docs/mysql/org-policy/custom-org-policy;
// the PostgreSQL and SQL Server pages list the same types).
const (
	TypeSQLInstance  = "sqladmin.googleapis.com/Instance"
	TypeSQLBackupRun = "sqladmin.googleapis.com/BackupRun"
)

func init() {
	RegisterFields(TypeSQLBackupRun, "description", "location")
	RegisterFields(TypeSQLInstance,
		"databaseVersion", "diskEncryptionConfiguration.kmsKeyName", "diskEncryptionStatus.kmsKeyVersionName",
		"failoverReplica.name", "masterInstanceName", "name", "nodeCount", "project", "region",
		"replicaConfiguration.cascadableReplica", "replicaConfiguration.failoverTarget",
		"settings.activationPolicy", "settings.activeDirectoryConfig.domain",
		"settings.advancedMachineFeatures.threadsPerCore", "settings.availabilityType",
		"settings.backupConfiguration.backupRetentionSettings.retainedBackups",
		"settings.backupConfiguration.backupRetentionSettings.retentionUnit",
		"settings.backupConfiguration.binaryLogEnabled", "settings.backupConfiguration.enabled",
		"settings.backupConfiguration.location", "settings.backupConfiguration.pointInTimeRecoveryEnabled",
		"settings.backupConfiguration.startTime", "settings.backupConfiguration.transactionLogRetentionDays",
		"settings.collation", "settings.connectionPoolConfig.connectionPoolingEnabled",
		"settings.connectionPoolConfig.flags.name", "settings.connectionPoolConfig.flags.value",
		"settings.connectorEnforcement", "settings.databaseFlags.name", "settings.databaseFlags.value",
		"settings.dataCacheConfig.dataCacheEnabled", "settings.dataDiskProvisionedIops",
		"settings.dataDiskProvisionedThroughput", "settings.dataDiskSizeGb", "settings.dataDiskType",
		"settings.deletionProtectionEnabled", "settings.denyMaintenancePeriods.endDate",
		"settings.denyMaintenancePeriods.startDate", "settings.denyMaintenancePeriods.time", "settings.edition",
		"settings.enableDataplexIntegration", "settings.enableGoogleMlIntegration",
		"settings.insightsConfig.queryInsightsEnabled", "settings.insightsConfig.queryPlansPerMinute",
		"settings.insightsConfig.queryStringLength", "settings.insightsConfig.recordApplicationTags",
		"settings.insightsConfig.recordClientAddress", "settings.ipConfiguration.authorizedNetworks.name",
		"settings.ipConfiguration.authorizedNetworks.value", "settings.ipConfiguration.customSubjectAlternativeNames",
		"settings.ipConfiguration.enablePrivatePathForGoogleCloudServices", "settings.ipConfiguration.ipv4Enabled",
		"settings.ipConfiguration.privateNetwork", "settings.ipConfiguration.pscConfig.allowedConsumerProjects",
		"settings.ipConfiguration.pscConfig.pscAutoConnections.consumerNetwork",
		"settings.ipConfiguration.pscConfig.pscAutoConnections.consumerProject",
		"settings.ipConfiguration.pscConfig.pscEnabled", "settings.ipConfiguration.serverCaMode",
		"settings.ipConfiguration.serverCaPool", "settings.ipConfiguration.sslMode",
		"settings.locationPreference.secondaryZone", "settings.locationPreference.zone",
		"settings.maintenanceWindow.day", "settings.maintenanceWindow.hour", "settings.maintenanceWindow.updateTrack",
		"settings.passwordValidationPolicy.complexity", "settings.passwordValidationPolicy.disallowUsernameSubstring",
		"settings.passwordValidationPolicy.enablePasswordPolicy", "settings.passwordValidationPolicy.minLength",
		"settings.passwordValidationPolicy.passwordChangeInterval", "settings.passwordValidationPolicy.reuseInterval",
		"settings.replicationLagMaxSeconds", "settings.retainBackupsOnDelete", "settings.sqlServerAuditConfig.bucket",
		"settings.sqlServerAuditConfig.retentionInterval", "settings.sqlServerAuditConfig.uploadInterval",
		"settings.storageAutoResize", "settings.tier", "settings.timeZone")
	RegisterTyped(TypeSQLInstance, map[string]string{
		"settings.passwordValidationPolicy.passwordChangeInterval": "duration",
		"settings.sqlServerAuditConfig.retentionInterval":          "duration",
		"settings.sqlServerAuditConfig.uploadInterval":             "duration",
	})
}
