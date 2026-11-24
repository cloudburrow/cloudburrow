package console

// Delete confirmations, per resource kind, as Google's console asks for each
// (#1931, docs/console-parity.md §4.8).
//
// Google's console does not confirm every delete the same way. A Pub/Sub
// topic's dialog asks for the word delete, a Cloud SQL instance's for the
// instance ID, and a Cloud Storage bucket's or a Cloud Run service's only
// for a click on Delete. The client draws whichever the provider's
// capability names (deleteConfirm), so each product's delete reads as
// Google's does, and one the docs say nothing about keeps the strictest:
// the name typed back.

// The confirmation styles.
const (
	// DeleteConfirmName asks for the resource's ID typed back: the default.
	DeleteConfirmName = "name"
	// DeleteConfirmWord asks for the word delete.
	DeleteConfirmWord = "delete"
	// DeleteConfirmWordUpper asks for the word DELETE.
	DeleteConfirmWordUpper = "DELETE"
	// DeleteConfirmWordTitle asks for the word Delete, as Cloud SQL's
	// Delete backup window does (#1860).
	DeleteConfirmWordTitle = "Delete"
	// DeleteConfirmClick asks for nothing typed: the dialog's Delete alone.
	DeleteConfirmClick = "confirm"
	// DeleteConfirmPrefix10 asks for the first 10 characters of the name,
	// as Cloud Storage's Delete HMAC key asks for the access ID's (#2047).
	// An action's ConfirmStyle only.
	DeleteConfirmPrefix10 = "prefix10"
	// DeleteConfirmDisable asks for the word Disable, as Storage
	// Intelligence's Disable does (#2047). An action's ConfirmStyle only.
	DeleteConfirmDisable = "Disable"
)

// DeleteConfirmer is a Deleter that names its confirmation itself, where
// the table below has no row for it.
type DeleteConfirmer interface {
	Deleter
	DeleteConfirm() string
}

// DeleteConfirmRule is one resource kind's documented confirmation.
type DeleteConfirmRule struct {
	// Style is one of the DeleteConfirm constants.
	Style string
	// Source is the Google doc whose console steps describe the dialog,
	// empty for an `UNVERIFIED` row.
	Source string
	// Verified is false where Google's docs do not describe the dialog: the
	// row then keeps DeleteConfirmName, the strictest, until they do.
	Verified bool
}

// unverified is a kind whose dialog Google's docs do not describe.
var unverified = DeleteConfirmRule{Style: DeleteConfirmName}

// DeleteConfirmRules is Google's documented delete confirmation for every
// provider (by ID) that can delete. A Deleter missing from it fails
// TestEveryDeleterHasADeleteConfirmRule, so a new product says which it is.
// Every row's docs, UNVERIFIED ones included, re-read 2026-11-19.
var DeleteConfirmRules = map[string]DeleteConfirmRule{
	// "Enter the project ID, and then click Shut down anyway."
	"projects": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects", Verified: true},
	// "type the folder ID or name to confirm and click Delete."
	"hierarchy": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/resource-manager/docs/manage-folders", Verified: true},
	// UNVERIFIED: no console steps for deleting a tag key (docs re-read 2026-11-19)
	"tags": unverified,
	// UNVERIFIED: Google's Analytics Hub docs describe no dialog for deleting a data exchange (docs re-read 2026-11-20: https://docs.cloud.google.com/bigquery/docs/analytics-hub-manage-exchanges)
	"bigquery-sharing": unverified,
	// More actions > Delete, then enter delete to confirm (docs read 2026-11-21).
	"bigquery-clean-rooms": {Style: DeleteConfirmWord, Source: "https://docs.cloud.google.com/bigquery/docs/data-clean-rooms", Verified: true},
	// Capacity management and BI Engine (#2193): Google's reservations,
	// commitments and BI Engine guides say "click Delete" and describe no
	// dialog (UNVERIFIED (re-read 2026-11-23:
	// https://docs.cloud.google.com/bigquery/docs/reservations-tasks,
	// https://docs.cloud.google.com/bigquery/docs/reservations-commitments,
	// https://docs.cloud.google.com/bigquery/docs/bi-engine-reserve-capacity)).
	"bigquery-capacity":           unverified,
	"bigquery-commitments":        unverified,
	"bigquery-reservation-groups": unverified,
	"bigquery-bi-engine":          unverified,
	// UNVERIFIED: "Select the checkbox for the roles ... click Delete." No dialog described (docs re-read 2026-11-19).
	"iam-roles": unverified,
	// "Confirm that you want to delete the group by clicking Confirm in the confirmation dialog."
	"iam-groups": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/iam/docs/groups-in-cloud-console", Verified: true},
	// UNVERIFIED: no console steps for deleting an SSO profile or an assignment (https://knowledge.workspace.google.com/admin/apps/setting-up-sso re-read 2026-11-20)
	"iam-sso-profiles":    unverified,
	"iam-sso-assignments": unverified,
	// UNVERIFIED (re-read 2026-11-21): the Admin console's Delete user,
	// Delete device, a policy's and an allowlisted domain's removal
	// describe no typed confirmation (https://knowledge.workspace.google.com/admin).
	"iam-users":               unverified,
	"iam-devices":             unverified,
	"iam-identity-policies":   unverified,
	"iam-allowlisted-domains": unverified,
	// UNVERIFIED: "Select the service account ... click Delete." No dialog described (docs re-read 2026-11-19).
	"iam-service-accounts": unverified,
	// a label is removed with its delete icon, then Save: nothing typed.
	"iam-labels": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/resource-manager/docs/creating-managing-labels", Verified: true},
	// "confirm you want to delete the bucket and its contents. Click Delete."
	"storage": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/storage/docs/deleting-buckets", Verified: true},
	// UNVERIFIED: no console steps for deleting a batch operations job (docs re-read 2026-11-19)
	"storage-batch-operations": unverified,
	// UNVERIFIED: "On the Report configuration details page, click Delete." No dialog described (docs re-read 2026-11-19).
	"storage-inventory-reports": unverified,
	// UNVERIFIED: "Select the dataset configuration ... click Delete." No dialog described (docs re-read 2026-11-19).
	"storage-insights-datasets": unverified,
	// UNVERIFIED: no console steps for deleting a transfer job read (docs re-read 2026-11-19)
	"storage-transfer": unverified,
	// UNVERIFIED: no console steps for deleting an agent pool read (docs re-read 2026-11-19)
	"storage-transfer-agent-pools": unverified,
	// "Enter delete and then click Delete."
	"pubsub": {Style: DeleteConfirmWord, Source: "https://docs.cloud.google.com/pubsub/docs/delete-topic", Verified: true},
	// UNVERIFIED: "Select the subscription to delete. Click Delete." No dialog described (docs re-read 2026-11-19).
	"pubsub-subscriptions": unverified,
	// UNVERIFIED: no console steps for deleting a snapshot (docs re-read 2026-11-19)
	"pubsub-snapshots": unverified,
	// "Click Delete. Confirm the delete operation."
	"pubsub-schemas": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/pubsub/docs/delete-schema", Verified: true},
	// "At the prompt, enter the queue name, and then click Confirm."
	"tasks": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/tasks/docs/manage-queues-and-tasks", Verified: true},
	// UNVERIFIED: "Click Delete. This deletes all revisions of the service." No dialog described (docs re-read 2026-11-19).
	"run": unverified,
	// UNVERIFIED: "select the domain mapping ... and click Delete." No dialog described (docs re-read 2026-11-19).
	"run-domains": unverified,
	// "Click Delete. Click Confirm"
	"run-jobs": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/run/docs/managing/jobs", Verified: true},
	// UNVERIFIED: "select the worker pool ... Click Delete." No dialog described (docs re-read 2026-11-19).
	"run-worker-pools": unverified,
	// UNVERIFIED: no console steps for deleting an instance (docs re-read 2026-11-19)
	"run-instances": unverified,
	// UNVERIFIED: "Select Delete." from the row menu; no dialog described (docs re-read 2026-11-19).
	"cloud-build-triggers": unverified,
	// UNVERIFIED: "click the trash icon." No dialog described (docs re-read 2026-11-19).
	"cloud-build-worker-pools": unverified,
	// UNVERIFIED: gcloud steps only (docs re-read 2026-11-19)
	"secure-source-manager": unverified,
	// UNVERIFIED: "From the More menu, select Delete." No dialog described (docs re-read 2026-11-22).
	"developer-connect-account-connectors": unverified,
	// UNVERIFIED: "select the repository to delete. Click Delete." No dialog described (docs re-read 2026-11-19).
	"artifact-registry": unverified,
	// UNVERIFIED: no console steps for deleting a function (docs re-read 2026-11-19)
	"functions": unverified,
	// UNVERIFIED: "Select the trigger and click Delete." No dialog described (docs re-read 2026-11-19).
	"eventarc": unverified,
	// Eventarc Advanced and channels (#1947).
	"eventarc-buses":              unverified,
	"eventarc-pipelines":          unverified,
	"eventarc-enrollments":        unverified,
	"eventarc-google-api-sources": unverified,
	"eventarc-channels":           unverified,
	// "enter the name of the secret, and then click Delete secret."
	"secrets": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/secret-manager/docs/delete-secrets", Verified: true},
	// "enter the name of the secret, and then click Delete secret."
	"secrets-regional": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/secret-manager/regional-secrets/delete-regional-secrets", Verified: true},
	// UNVERIFIED: "Select the job to delete. Click Delete." No dialog described (docs re-read 2026-11-19).
	"scheduler": unverified,
	// UNVERIFIED: "Select the metric ... and click Delete." No dialog described (docs re-read 2026-11-19).
	"logging-metrics": unverified,
	// UNVERIFIED: "Delete sink" named; no dialog described (docs re-read 2026-11-19).
	"logging-sinks": unverified,
	// UNVERIFIED: an exclusion is removed by editing its sink; no dialog described (docs re-read 2026-11-19).
	"logging-exclusions": unverified,
	// "Select Delete bucket. On the confirmation panel, click Delete."
	"logging-buckets": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/logging/docs/buckets", Verified: true},
	// UNVERIFIED: "select Delete." No dialog described (docs re-read 2026-11-19).
	"logging-saved-queries": unverified,
	// UNVERIFIED: "select Delete scope, and then complete the dialog." Its contents not described (docs re-read 2026-11-19).
	"logging-log-scopes": unverified,
	// UNVERIFIED: "click More, and then select Delete scope." No dialog described (docs re-read 2026-11-21).
	"logging-trace-scopes": unverified,
	// "confirm deletion by typing the database ID in the text field."
	"firestore-databases": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/firestore/native/docs/manage-databases", Verified: true},
	// "Click Delete, and then confirm you want to delete the Task entity."
	"datastore": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/datastore/docs/store-query-data", Verified: true},
	// "type the table ID in the Delete table box. Click Delete."
	"bigtable": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/bigtable/docs/managing-tables", Verified: true},
	// UNVERIFIED: "Follow the instructions in the confirmation dialog" without saying what they ask (docs re-read 2026-11-19).
	"bigtable-instances": unverified,
	// "enter the name of the database and then click Delete."
	"cloudsql": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/sql/docs/mysql/create-manage-databases", Verified: true},
	// "enter the name of the database and then click Delete."
	"cloudsql-mysql": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/sql/docs/mysql/create-manage-databases", Verified: true},
	// "In the text field, enter the instance ID that appears."
	"cloudsql-instances": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/sql/docs/postgres/delete-instance", Verified: true},
	// "To confirm deletion, enter the instance's ID."
	"memorystore": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/memorystore/docs/redis/create-manage-instances", Verified: true},
	// "In the dialog, enter the cluster ID, and then click Delete."
	"memorystore-clusters": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/memorystore/docs/cluster/delete-instances", Verified: true},
	// "In the dialog, enter the instance ID, and then click Delete."
	"memorystore-valkey": {Style: DeleteConfirmName, Source: "https://docs.cloud.google.com/memorystore/docs/valkey/delete-instances", Verified: true},
	// "In the Delete dataset dialog, type delete into the field, and then click Delete."
	// UNVERIFIED: the scheduling-queries and transfer pages say only "click Delete"; no dialog described (docs re-read 2026-11-19).
	"bigquery-transfers": unverified,
	// UNVERIFIED: as bigquery-transfers (docs re-read 2026-11-19).
	"bigquery-scheduled-queries": unverified,
	"bigquery":                   {Style: DeleteConfirmWord, Source: "https://docs.cloud.google.com/bigquery/docs/managing-datasets", Verified: true},
	// UNVERIFIED: no console delete for a job's history (docs re-read 2026-11-19)
	"bigquery-jobs": unverified,
	// UNVERIFIED: "Next to your processor, in the Action menu, click Delete
	// processor." No dialog described (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/document-ai/docs/create-processor).
	"documentai": unverified,
	// UNVERIFIED: no console steps for deleting a recognizer, custom class or
	// phrase set (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/speech-to-text/docs/recognizers).
	"speech-recognizers":    unverified,
	"speech-custom-classes": unverified,
	"speech-phrase-sets":    unverified,
	// Workload Identity Federation (#1941): "Click Delete pool, then click
	// Delete." and, for a provider, "Click Delete to delete the provider."
	"iam-workload-identity": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/iam/docs/manage-workload-identity-pools-providers", Verified: true},
	// UNVERIFIED (re-read 2026-11-20): Workforce Identity Federation
	// (#2056): "click Delete on the pool that you want to delete" and
	// "Follow additional instructions"; the dialog itself is not described
	// (https://docs.cloud.google.com/iam/docs/manage-workforce-identity-pools-providers).
	"iam-workforce-identity": unverified,
	// UNVERIFIED (re-read 2026-11-20): principal access boundary policies
	// (#2057): the console steps for deleting a policy or a binding are not
	// described (https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies-create).
	"iam-principal-access-boundary": unverified,
	// Model Registry (#1940): "Click Delete on the confirmation screen."
	"vertex-models": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/vertex-ai/docs/model-registry/delete-model", Verified: true},
	// UNVERIFIED: "click Actions, and then click Delete endpoint." No dialog
	// described (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/vertex-ai/docs/predictions/undeploy-model).
	"vertex-endpoints": unverified,
	// Colab Enterprise runtimes (#2105): "select the runtime ... click
	// Delete ... click Confirm" (https://docs.cloud.google.com/colab/docs/manage-runtimes).
	"colab-runtimes": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/colab/docs/manage-runtimes", Verified: true},
	// Colab Enterprise executions (#2105): "Select the notebook run that you
	// want to delete the result for. Click Delete. To confirm the deletion,
	// click Confirm." (https://docs.cloud.google.com/colab/docs/schedule-notebook-run).
	"colab-executions": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/colab/docs/schedule-notebook-run", Verified: true},
	// UNVERIFIED (re-read 2026-11-22): "Click Pause, Resume, or Delete." for a
	// schedule, no dialog described
	// (https://docs.cloud.google.com/colab/docs/schedule-notebook-run).
	"colab-schedules": unverified,
	// Vertex AI Pipelines' schedules (#2173): "click Delete, then click
	// Delete again to confirm" from the row's options menu
	// (https://docs.cloud.google.com/vertex-ai/docs/pipelines/schedule-pipeline-run).
	"vertex-pipeline-schedules": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/vertex-ai/docs/pipelines/schedule-pipeline-run", Verified: true},
	// UNVERIFIED (re-read 2026-11-22): deleting a pipeline run from the
	// console is not described (run-pipeline, schedule-pipeline-run).
	"vertex-pipeline-runs": unverified,
	// Agent Platform Model Monitoring (#2214): Google's docs show no
	// delete dialog for a monitor, a monitoring schedule or a v1 job.
	"vertex-model-monitors":              unverified,
	"vertex-model-monitoring-schedules":  unverified,
	"vertex-model-deployment-monitoring": unverified,
	// Vertex AI Workbench instances (#2173): "click Delete ... To confirm,
	// click Confirm"
	// (https://docs.cloud.google.com/vertex-ai/docs/workbench/instances/create-console-quickstart).
	"workbench-instances": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/vertex-ai/docs/workbench/instances/create-console-quickstart", Verified: true},
	// My notebooks (#2105): "click the Actions menu, and then select Delete
	// notebook. In the Delete notebook dialog, click Confirm."
	// (https://docs.cloud.google.com/colab/docs/create-console-quickstart).
	"colab-notebooks": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/colab/docs/create-console-quickstart", Verified: true},
	// UNVERIFIED (re-read 2026-11-22): Google's console documents no Delete
	// for a runtime template, which the API has
	// (https://docs.cloud.google.com/colab/docs/create-runtime-template).
	"colab-templates": unverified,
	// UNVERIFIED: Metrics management documents deleting exclusion rules
	// only, not a metric (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/monitoring/metrics-management).
	"monitoring-metrics": unverified,
	// "In the confirmation dialog, select Delete."
	"monitoring-alerting": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/monitoring/alerts/manage-alerts", Verified: true},
	// "Click Delete in the confirmation dialog."
	"monitoring-channels": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/monitoring/support/notification-options", Verified: true},
	// UNVERIFIED: "open its More menu and choose Delete". No dialog
	// described (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/monitoring/uptime-checks/manage).
	"monitoring-uptime": unverified,
	// "In the confirmation dialog, click Delete."
	"monitoring-dashboards": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/monitoring/charts/dashboards", Verified: true},
	// "In the Delete group and subgroup dialog, click Delete".
	"monitoring-groups": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/monitoring/groups", Verified: true},
	// UNVERIFIED: Delete service is offered for custom services; no dialog
	// described (docs re-read 2026-11-19:
	// https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/ui/svc-overview).
	"monitoring-services": unverified,
	// VPC network (#1936): "In the message that appears, click Delete to
	// confirm."
	"vpc-networks": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/vpc/docs/create-modify-vpc-networks", Verified: true},
	// "Click Delete again to confirm."
	"vpc-firewalls": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/firewall/docs/using-firewalls", Verified: true},
	// "Click Delete again to confirm."
	"vpc-routes": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/vpc/docs/using-routes", Verified: true},
	// Service connection policies (#2141). UNVERIFIED: the dialog is not
	// described (docs re-read 2026-11-22:
	// https://docs.cloud.google.com/vpc/docs/configure-service-connection-policies).
	"psc-policies": unverified,
	// Compute Engine snapshots (#1965). UNVERIFIED: "Select one or more
	// snapshots that you want to delete. At the top of the Snapshots page,
	// click Delete." No dialog described (docs re-read 2026-11-20:
	// https://docs.cloud.google.com/compute/docs/disks/manage-snapshots).
	"compute-snapshots": unverified,
	// Compute Engine machine images (#2139). UNVERIFIED: "Select the
	// machine images you want to delete, then click Delete." No dialog
	// described (docs re-read 2026-11-22:
	// https://docs.cloud.google.com/compute/docs/machine-images/manage-machine-images).
	"compute-machine-images": unverified,
}

// deleteConfirmOf is p's confirmation style, empty for the default.
func deleteConfirmOf(p Provider) string {
	if dc, ok := p.(DeleteConfirmer); ok {
		return normalDeleteConfirm(dc.DeleteConfirm())
	}
	if r, ok := DeleteConfirmRules[p.ID()]; ok {
		return normalDeleteConfirm(r.Style)
	}
	return ""
}

// normalDeleteConfirm is style as the capability carries it: empty for the
// default, so a provider that says nothing sends nothing.
func normalDeleteConfirm(style string) string {
	switch style {
	case DeleteConfirmWord, DeleteConfirmWordUpper, DeleteConfirmClick:
		return style
	}
	return ""
}
