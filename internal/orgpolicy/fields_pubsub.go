package orgpolicy

// Pub/Sub's documented fields ("Use custom organization policies",
// Pub/Sub, Supported resources, read 2026-11-19).
func init() {
	RegisterFields(TypeSchema, "definition", "name", "type")
	RegisterFields(TypeSnapshot, "expireTime", "name", "topic")
	RegisterTyped(TypeSnapshot, map[string]string{"expireTime": "timestamp"})
	RegisterFields(TypeSubscription,
		"ackDeadlineSeconds",
		"bigqueryConfig.dropUnknownFields", "bigqueryConfig.serviceAccountEmail", "bigqueryConfig.table",
		"bigqueryConfig.useTableSchema", "bigqueryConfig.useTopicSchema", "bigqueryConfig.writeMetadata",
		"bigtableConfig.appProfileId", "bigtableConfig.serviceAccountEmail", "bigtableConfig.table", "bigtableConfig.writeMetadata",
		"cloudStorageConfig.avroConfig.useTopicSchema", "cloudStorageConfig.avroConfig.writeMetadata",
		"cloudStorageConfig.bucket", "cloudStorageConfig.filenameDatetimeFormat", "cloudStorageConfig.filenamePrefix",
		"cloudStorageConfig.filenameSuffix", "cloudStorageConfig.maxBytes", "cloudStorageConfig.maxDuration",
		"cloudStorageConfig.maxMessages", "cloudStorageConfig.serviceAccountEmail",
		"deadLetterPolicy.deadLetterTopic", "deadLetterPolicy.maxDeliveryAttempts",
		"enableExactlyOnceDelivery", "enableMessageOrdering", "expirationPolicy.ttl", "filter", "messageRetentionDuration",
		"messageTransforms.aiInference.endpoint", "messageTransforms.aiInference.serviceAccountEmail",
		"messageTransforms.aiInference.unstructuredInference.parameters", "messageTransforms.disabled",
		"messageTransforms.enabled", "messageTransforms.javascriptUdf.code", "messageTransforms.javascriptUdf.functionName",
		"name", "pushConfig.attributes", "pushConfig.noWrapper.writeMetadata", "pushConfig.oidcToken.audience",
		"pushConfig.oidcToken.serviceAccountEmail", "pushConfig.pushEndpoint", "retainAckedMessages",
		"retryPolicy.maximumBackoff", "retryPolicy.minimumBackoff", "topic")
	RegisterFields(TypeTopic,
		"ingestionDataSourceSettings.awsKinesis.awsRoleArn", "ingestionDataSourceSettings.awsKinesis.consumerArn",
		"ingestionDataSourceSettings.awsKinesis.gcpServiceAccount", "ingestionDataSourceSettings.awsKinesis.streamArn",
		"ingestionDataSourceSettings.awsMsk.awsRoleArn", "ingestionDataSourceSettings.awsMsk.clusterArn",
		"ingestionDataSourceSettings.awsMsk.gcpServiceAccount", "ingestionDataSourceSettings.awsMsk.topic",
		"ingestionDataSourceSettings.azureEventHubs.clientId", "ingestionDataSourceSettings.azureEventHubs.eventHub",
		"ingestionDataSourceSettings.azureEventHubs.gcpServiceAccount", "ingestionDataSourceSettings.azureEventHubs.namespace",
		"ingestionDataSourceSettings.azureEventHubs.resourceGroup", "ingestionDataSourceSettings.azureEventHubs.subscriptionId",
		"ingestionDataSourceSettings.azureEventHubs.tenantId", "ingestionDataSourceSettings.cloudStorage.bucket",
		"ingestionDataSourceSettings.cloudStorage.matchGlob", "ingestionDataSourceSettings.cloudStorage.minimumObjectCreateTime",
		"ingestionDataSourceSettings.cloudStorage.textFormat.delimiter",
		"ingestionDataSourceSettings.confluentCloud.bootstrapServer", "ingestionDataSourceSettings.confluentCloud.clusterId",
		"ingestionDataSourceSettings.confluentCloud.gcpServiceAccount", "ingestionDataSourceSettings.confluentCloud.identityPoolId",
		"ingestionDataSourceSettings.confluentCloud.topic", "ingestionDataSourceSettings.platformLogsSettings.severity",
		"kmsKeyName", "messageRetentionDuration", "messageStoragePolicy.allowedPersistenceRegions",
		"messageStoragePolicy.enforceInTransit",
		"messageTransforms.aiInference.endpoint", "messageTransforms.aiInference.serviceAccountEmail",
		"messageTransforms.aiInference.unstructuredInference.parameters", "messageTransforms.disabled",
		"messageTransforms.enabled", "messageTransforms.javascriptUdf.code", "messageTransforms.javascriptUdf.functionName",
		"name", "schemaSettings.encoding", "schemaSettings.firstRevisionId", "schemaSettings.lastRevisionId",
		"schemaSettings.schema")
}
