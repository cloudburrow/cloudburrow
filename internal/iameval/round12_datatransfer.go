package iameval

// DataTransfer is the BigQuery Data Transfer Service's table (#1879,
// round 12), each method's permission from its REST reference
// (https://docs.cloud.google.com/bigquery/docs/reference/datatransfer/rest/v1,
// read 2026-11-20): bigquery.transfers.get to read a data source, a
// transfer config, its runs and their logs, and to check a data source's
// credentials; bigquery.transfers.update to create or delete a config,
// schedule or start runs, and delete a run; resourcemanager.projects.update
// on the project to enroll or unenroll data sources. transferConfigs.patch
// names no permission on its page; "Enable the BigQuery Data Transfer
// Service" (https://docs.cloud.google.com/bigquery/docs/enable-transfer-service)
// names bigquery.transfers.update to modify a transfer, which is checked.
// A config's serviceAccountName needs "permissions to act as this service
// account" (create and patch pages): iam.serviceAccounts.actAs. The
// Locations mixin's pages name no permission and are not checked. The
// service keeps no resource-level policies. Google's wording of a denial
// is UNVERIFIED (the pages give none): newerDeny's.
var DataTransfer = &Service{
	Name: "bigquerydatatransfer.googleapis.com",
	Methods: methodRows("/google.cloud.bigquery.datatransfer.v1.DataTransferService/", map[string]Method{
		"GetDataSource":   {Permission: "bigquery.transfers.get", Field: "name"},
		"ListDataSources": {Permission: "bigquery.transfers.get", Field: "parent"},
		"CheckValidCreds": {Permission: "bigquery.transfers.get", Field: "name"},
		"CreateTransferConfig": {Permission: "bigquery.transfers.update", Field: "parent",
			ActAs: []string{"service_account_name"}},
		"UpdateTransferConfig": {Permission: "bigquery.transfers.update", Field: "transfer_config.name",
			ActAs: []string{"service_account_name"}},
		"DeleteTransferConfig":    {Permission: "bigquery.transfers.update", Field: "name"},
		"GetTransferConfig":       {Permission: "bigquery.transfers.get", Field: "name"},
		"ListTransferConfigs":     {Permission: "bigquery.transfers.get", Field: "parent"},
		"ScheduleTransferRuns":    {Permission: "bigquery.transfers.update", Field: "parent"},
		"StartManualTransferRuns": {Permission: "bigquery.transfers.update", Field: "parent"},
		"GetTransferRun":          {Permission: "bigquery.transfers.get", Field: "name"},
		"DeleteTransferRun":       {Permission: "bigquery.transfers.update", Field: "name"},
		"ListTransferRuns":        {Permission: "bigquery.transfers.get", Field: "parent"},
		"ListTransferLogs":        {Permission: "bigquery.transfers.get", Field: "parent"},
		"EnrollDataSources":       {Permission: "resourcemanager.projects.update", Field: "name"},
		"UnenrollDataSources":     {Permission: "resourcemanager.projects.update", Field: "name"},
	}),
	Deny: newerDeny,
}

func init() {
	Services["google.cloud.bigquery.datatransfer.v1.DataTransferService"] = DataTransfer
}
