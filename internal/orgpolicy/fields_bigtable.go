package orgpolicy

// Bigtable's custom constraint resource types and their documented fields
// ("Use custom organization policies", Bigtable, Supported resources,
// https://docs.cloud.google.com/bigtable/docs/custom-constraints, read
// 2026-11-19).
const (
	TypeBigtableAppProfile       = "bigtableadmin.googleapis.com/AppProfile"
	TypeBigtableBackup           = "bigtableadmin.googleapis.com/Backup"
	TypeBigtableCluster          = "bigtableadmin.googleapis.com/Cluster"
	TypeBigtableInstance         = "bigtableadmin.googleapis.com/Instance"
	TypeBigtableLogicalView      = "bigtableadmin.googleapis.com/LogicalView"
	TypeBigtableMaterializedView = "bigtableadmin.googleapis.com/MaterializedView"
	TypeBigtableSchemaBundle     = "bigtableadmin.googleapis.com/SchemaBundle"
	TypeBigtableTable            = "bigtableadmin.googleapis.com/Table"
)

func init() {
	RegisterFields(TypeBigtableAppProfile,
		"dataBoostIsolationReadOnly.computeBillingOwner", "description", "multiClusterRoutingUseAny.clusterIds", "name",
		"singleClusterRouting.allowTransactionalWrites", "singleClusterRouting.clusterId", "standardIsolation.priority")
	RegisterFields(TypeBigtableBackup, "backupType", "expireTime", "hotToStandardTime", "name", "sourceTable")
	RegisterTyped(TypeBigtableBackup, map[string]string{"expireTime": "timestamp", "hotToStandardTime": "timestamp"})
	RegisterFields(TypeBigtableCluster,
		"clusterConfig.clusterAutoscalingConfig.autoscalingLimits.maxServeNodes",
		"clusterConfig.clusterAutoscalingConfig.autoscalingLimits.minServeNodes",
		"clusterConfig.clusterAutoscalingConfig.autoscalingTargets.cpuUtilizationPercent",
		"clusterConfig.clusterAutoscalingConfig.autoscalingTargets.storageUtilizationGibPerNode",
		"defaultStorageType", "encryptionConfig.kmsKeyName", "location", "name", "nodeScalingFactor", "serveNodes")
	RegisterFields(TypeBigtableInstance, "displayName", "name")
	RegisterFields(TypeBigtableLogicalView, "deletionProtection", "name")
	RegisterFields(TypeBigtableMaterializedView, "deletionProtection", "name")
	RegisterFields(TypeBigtableSchemaBundle, "name", "protoSchema.protoDescriptors")
	RegisterFields(TypeBigtableTable, "automatedBackupPolicy.frequency", "automatedBackupPolicy.locations",
		"automatedBackupPolicy.retentionPeriod", "deletionProtection", "name")
	RegisterTyped(TypeBigtableTable, map[string]string{"automatedBackupPolicy.frequency": "duration",
		"automatedBackupPolicy.retentionPeriod": "duration"})
}
