package orgpolicy

// Spanner's custom constraint resource types and their documented fields
// ("Add a custom organization policy", Spanner, Supported Spanner
// resources, https://docs.cloud.google.com/spanner/docs/spanner-custom-constraints,
// read 2026-11-19).
const (
	TypeSpannerBackup         = "spanner.googleapis.com/Backup"
	TypeSpannerDatabase       = "spanner.googleapis.com/Database"
	TypeSpannerInstance       = "spanner.googleapis.com/Instance"
	TypeSpannerInstanceConfig = "spanner.googleapis.com/InstanceConfig"
)

func init() {
	RegisterFields(TypeSpannerBackup, "database", "expireTime", "name", "versionTime")
	RegisterTyped(TypeSpannerBackup, map[string]string{"expireTime": "timestamp", "versionTime": "timestamp"})
	RegisterFields(TypeSpannerDatabase, "enableDropProtection", "name")
	RegisterFields(TypeSpannerInstance,
		"autoscalingConfig.autoscalingLimits.maxNodes", "autoscalingConfig.autoscalingLimits.maxProcessingUnits",
		"autoscalingConfig.autoscalingLimits.minNodes", "autoscalingConfig.autoscalingLimits.minProcessingUnits",
		"autoscalingConfig.autoscalingTargets.highPriorityCpuUtilizationPercent",
		"autoscalingConfig.autoscalingTargets.storageUtilizationPercent",
		"config", "displayName", "freeInstanceMetadata.expireBehavior", "instanceType", "name", "nodeCount",
		"processingUnits")
	RegisterFields(TypeSpannerInstanceConfig, "baseConfig", "displayName", "leaderOptions", "name",
		"replicas.defaultLeaderLocation", "replicas.location", "replicas.type")
}
