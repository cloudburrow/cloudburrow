package orgpolicy

// BigQuery's custom constraint resource types and their documented fields
// ("Use organization policies to apply custom constraints to BigQuery
// resources", Supported resources, read 2026-11-19,
// https://docs.cloud.google.com/bigquery/docs/custom-constraints).
// bigquerydatapolicy.googleapis.com/DataPolicy and the
// bigqueryreservation.googleapis.com types are documented too; CloudBurrow
// serves neither API.
const (
	TypeBigQueryDataset         = "bigquery.googleapis.com/Dataset"
	TypeBigQueryModel           = "bigquery.googleapis.com/Model"
	TypeBigQueryRoutine         = "bigquery.googleapis.com/Routine"
	TypeBigQueryRowAccessPolicy = "bigquery.googleapis.com/RowAccessPolicy"
	TypeBigQueryTable           = "bigquery.googleapis.com/Table"
)

func init() {
	RegisterFields(TypeBigQueryDataset,
		"datasetReference.datasetId", "defaultCollation", "defaultEncryptionConfiguration.kmsKeyName",
		"defaultPartitionExpirationMs", "defaultRoundingMode", "defaultTableExpirationMs", "description",
		"externalCatalogDatasetOptions.defaultStorageLocationUri", "externalCatalogDatasetOptions.parameters",
		"externalDatasetReference.connection", "externalDatasetReference.externalSource", "friendlyName",
		"isCaseInsensitive", "linkedDatasetSource.sourceDataset.datasetId", "location", "maxTimeTravelHours",
		"storageBillingModel")
	RegisterFields(TypeBigQueryModel, "modelReference.datasetId", "modelReference.modelId", "modelReference.projectId")
	RegisterFields(TypeBigQueryRoutine, "routineReference.datasetId", "routineReference.projectId", "routineReference.routineId")
	RegisterFields(TypeBigQueryRowAccessPolicy, "rowAccessPolicyReference.datasetId", "rowAccessPolicyReference.policyId",
		"rowAccessPolicyReference.projectId", "rowAccessPolicyReference.tableId")
	RegisterFields(TypeBigQueryTable, "tableReference.datasetId", "tableReference.projectId", "tableReference.tableId")
}
