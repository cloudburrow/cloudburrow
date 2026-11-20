package storagejobs

// The linked dataset's views and their schemas, as Google documents them
// (docs.cloud.google.com/storage/docs/insights/dataset-tables-and-schemas,
// read 2026-11-19). Each view reads a table of its own, which the front
// leaves out of tables.list (bigqueryfront/loglinks.go): the views are what
// Google's linked dataset holds.

// Field is one column of a linked table, as BigQuery's schema names it.
type Field struct {
	Name     string
	Type     string
	Repeated bool
	Fields   []Field
}

// LinkedTable is a table of a linked dataset with its rows.
type LinkedTable struct {
	ID          string
	Description string
	Schema      []Field
}

// LinkedView is a view of a linked dataset.
type LinkedView struct {
	ID          string
	Description string
	Query       string
}

func col(name, typ string) Field { return Field{Name: name, Type: typ} }

func rec(name string, fields ...Field) Field {
	return Field{Name: name, Type: "RECORD", Fields: fields}
}

func reps(name string, fields ...Field) Field {
	return Field{Name: name, Type: "RECORD", Repeated: true, Fields: fields}
}

func keyValue(name string) Field { return reps(name, col("key", "STRING"), col("value", "STRING")) }

var bucketAttributesSchema = []Field{
	col("snapshotTime", "TIMESTAMP"), col("name", "STRING"), col("location", "STRING"), col("project", "INTEGER"),
	col("storageClass", "STRING"),
	rec("public", col("bucketPolicyOnly", "BOOLEAN"), col("publicAccessPrevention", "STRING")),
	rec("autoclass", col("enabled", "BOOLEAN"), col("toggleTime", "TIMESTAMP")),
	col("versioning", "BOOLEAN"), col("lifecycle", "BOOLEAN"), col("metageneration", "INTEGER"), col("timeCreated", "TIMESTAMP"),
	rec("tags", col("lastUpdatedTime", "TIMESTAMP"), keyValue("tagMap")),
	keyValue("labels"),
	col("retentionPeriod", "INTEGER"), col("retentionMode", "STRING"), col("objectRetentionEnabled", "BOOLEAN"),
	col("defaultEventBasedHold", "BOOLEAN"),
	rec("softDeletePolicy", col("effectiveTime", "DATETIME"), col("retentionDurationSeconds", "INTEGER")),
	{Name: "enforcedEncryptionAllowedTypes", Type: "STRING", Repeated: true},
	rec("encryption", col("defaultEncryptionType", "STRING"), col("defaultKmsKeyName", "STRING")),
	rec("iamConfiguration", rec("uniformBucketLevelAccess", col("enabled", "BOOLEAN")), col("publicAccessPrevention", "STRING")),
	keyValue("resourceTags"),
	col("objectCount", "INTEGER"), col("totalSize", "INTEGER"),
}

var objectAttributesSchema = []Field{
	col("snapshotTime", "TIMESTAMP"), col("bucket", "STRING"), col("location", "STRING"), col("componentCount", "INTEGER"),
	col("contentDisposition", "STRING"), col("contentEncoding", "STRING"), col("contentLanguage", "STRING"), col("contentType", "STRING"),
	reps("contexts", col("key", "STRING"), col("value", "STRING"), col("type", "STRING"), col("createTime", "TIMESTAMP"),
		col("updateTime", "TIMESTAMP")),
	col("crc32c", "INTEGER"), col("customTime", "TIMESTAMP"), col("etag", "STRING"), col("eventBasedHold", "BOOLEAN"),
	col("generation", "INTEGER"), col("md5Hash", "STRING"), col("mediaLink", "STRING"), keyValue("metadata"),
	col("metageneration", "INTEGER"), col("name", "STRING"), col("selfLink", "STRING"), col("size", "INTEGER"),
	col("storageClass", "STRING"), col("temporaryHold", "BOOLEAN"), col("timeCreated", "TIMESTAMP"), col("timeDeleted", "TIMESTAMP"),
	col("updated", "TIMESTAMP"), col("timeStorageClassUpdated", "TIMESTAMP"), col("retentionExpirationTime", "TIMESTAMP"),
	col("retentionExpirationTimeLockStatus", "BOOLEAN"), col("retentionExpirationTimeSource", "STRING"),
	col("retainUntilTime", "TIMESTAMP"), col("retainUntilTimeLockStatus", "BOOLEAN"),
	col("softDeleteTime", "DATETIME"), col("hardDeleteTime", "DATETIME"), col("project", "INTEGER"),
	rec("ref", col("uri", "STRING"), col("version", "STRING"), col("authorizer", "STRING"), col("details", "JSON")),
	col("encryptionType", "STRING"),
	rec("securityInsights", rec("publicAccessInsight", col("readPublicAccess", "STRING"), col("readPublicAccessSource", "STRING"),
		col("writePublicAccess", "STRING"))),
}

var projectAttributesSchema = []Field{
	col("snapshotTime", "TIMESTAMP"), col("name", "STRING"), col("id", "STRING"), col("number", "INTEGER"),
}

var objectContextsCatalogSchema = []Field{
	col("project", "INTEGER"), col("bucket", "STRING"), col("name", "STRING"), col("generation", "INTEGER"), col("location", "STRING"),
	col("createdTime", "STRING"), col("catalogUpdatedTime", "TIMESTAMP"),
	reps("contexts", col("key", "STRING"), col("value", "STRING"), col("contextCreationTime", "TIMESTAMP"),
		col("contextUpdateTime", "TIMESTAMP"), col("extendedData", "JSON")),
}

var eventsSchema = []Field{
	rec("manifest", col("snapshotTime", "TIMESTAMP"), col("viewName", "STRING"), col("location", "STRING")),
	rec("globalManifest", col("snapshotTime", "TIMESTAMP")),
	col("eventTime", "TIMESTAMP"), col("eventCode", "STRING"),
}

var errorAttributesSchema = []Field{
	col("errorCode", "INTEGER"), col("errorSource", "STRING"), col("errorTime", "TIMESTAMP"), col("sourceGcsLocation", "STRING"),
	rec("bucketErrorRecord", col("bucketName", "STRING"), col("serviceAccount", "STRING")),
	rec("projectErrorRecord", col("projectNumber", "INTEGER"), col("organizationName", "STRING")),
}

// The activity views (activity.go, #1979). Google describes each of
// bucket_activity_view's topPrefixes* columns as a repeated RECORD of "the
// prefix as a STRING and the count as an INTEGER" without naming the two
// fields; they are prefix and count here (UNVERIFIED names, re-read 2026-11-19,
// https://docs.cloud.google.com/storage/docs/insights/dataset-tables-and-schemas).
var objectEventsSchema = []Field{
	col("requestCompletionTimestamp", "TIMESTAMP"), col("project", "INTEGER"), col("bucketName", "STRING"), col("objectName", "STRING"),
	col("requestOperation", "STRING"), col("requestBytes", "INTEGER"), col("responseBytes", "INTEGER"),
	col("requestProcessingTimeMicros", "INTEGER"), col("requestId", "STRING"), col("requestHost", "STRING"),
	col("requestHttpMethod", "STRING"), col("responseStatus", "INTEGER"), col("requestReferrer", "STRING"), col("size", "INTEGER"),
	col("generation", "INTEGER"), col("timeCreated", "TIMESTAMP"), col("storageClass", "STRING"), col("errorReason", "STRING"),
	col("location", "STRING"),
}

func activityCounts() []Field {
	var out []Field
	for _, n := range []string{"totalRequests", "totalReads", "totalWrites", "totalUpdates", "totalDeletes", "totalLists",
		"totalRequestBytes", "totalResponseBytes", "count4xx", "count5xx", "count2xx", "count400", "count401", "count403", "count429"} {
		out = append(out, col(n, "INTEGER"))
	}
	return out
}

var projectActivitySchema = append([]Field{col("snapshotStartTime", "TIMESTAMP"), col("snapshotEndTime", "TIMESTAMP"),
	col("project", "INTEGER")}, activityCounts()...)

func topPrefixesField(name string) Field {
	return reps(name, col("prefix", "STRING"), col("count", "INTEGER"))
}

var bucketActivitySchema = append(append([]Field{col("snapshotStartTime", "TIMESTAMP"), col("snapshotEndTime", "TIMESTAMP"),
	col("name", "STRING"), col("generation", "INTEGER"), col("project", "INTEGER"), col("location", "STRING")}, activityCounts()...),
	topPrefixesField("topPrefixes429Errors"), topPrefixesField("topPrefixesRequestBytes"),
	topPrefixesField("topPrefixesResponseBytes"), topPrefixesField("topPrefixesHighestOperations"))

var bucketRegionActivitySchema = []Field{
	col("snapshotStartTime", "TIMESTAMP"), col("snapshotEndTime", "TIMESTAMP"), col("project", "INTEGER"), col("name", "STRING"),
	col("generation", "INTEGER"), col("requestLocation", "STRING"), col("bucketLocation", "STRING"), col("requestBytes", "INTEGER"),
	col("responseBytes", "INTEGER"),
}

// insightsTable is one table of a linked dataset and the views over it.
type insightsTable struct {
	key    string // the snapshot's rows of it
	schema []Field
	// view reads every retained row; latest, when set, the latest
	// snapshot's (by snapshotTime).
	view, latest string
	desc         string
}

// insightsTables are the linked dataset's tables, in the order made.
var insightsTables = []insightsTable{
	{key: "bucket_attributes", schema: bucketAttributesSchema, view: "bucket_attributes_view",
		latest: "bucket_attributes_latest_snapshot_view", desc: "Cloud Storage bucket metadata, one row per bucket per snapshot."},
	{key: "object_attributes", schema: objectAttributesSchema, view: "object_attributes_view",
		latest: "object_attributes_latest_snapshot_view", desc: "Cloud Storage object metadata, one row per object per snapshot."},
	{key: "project_attributes", schema: projectAttributesSchema, view: "project_attributes_view",
		desc: "Project metadata, one row per project per snapshot."},
	{key: "object_contexts_catalog", schema: objectContextsCatalogSchema, view: "object_contexts_catalog_view",
		desc: "The latest contexts of objects that have contexts."},
	{key: "events", schema: eventsSchema, view: "events_view", desc: "Snapshot processing events."},
	{key: "error_attributes", schema: errorAttributesSchema, view: "error_attributes_view",
		desc: "Errors met while taking snapshots."},
	{key: "object_events", schema: objectEventsSchema, view: "object_events_view", desc: "Object mutation and error activity."},
	{key: "project_activity", schema: projectActivitySchema, view: "project_activity_view", desc: "Project activity."},
	{key: "bucket_activity", schema: bucketActivitySchema, view: "bucket_activity_view", desc: "Bucket activity."},
	{key: "bucket_region_activity", schema: bucketRegionActivitySchema, view: "bucket_region_activity_view",
		desc: "Bucket activity by request region."},
}

// hiddenTablePrefix starts the name of each table the views read; the
// BigQuery front leaves such tables out of a linked dataset's tables.list.
const hiddenTablePrefix = "_cloudburrow_"

func (t insightsTable) tableID() string { return hiddenTablePrefix + t.key }
