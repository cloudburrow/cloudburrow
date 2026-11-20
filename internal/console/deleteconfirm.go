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
	// DeleteConfirmClick asks for nothing typed: the dialog's Delete alone.
	DeleteConfirmClick = "confirm"
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
	// UNVERIFIED: "Select the checkbox for the roles ... click Delete." No dialog described (docs re-read 2026-11-19).
	"iam-roles": unverified,
	// "Confirm that you want to delete the group by clicking Confirm in the confirmation dialog."
	"iam-groups": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/iam/docs/groups-in-cloud-console", Verified: true},
	// UNVERIFIED: "Select the service account ... click Delete." No dialog described (docs re-read 2026-11-19).
	"iam-service-accounts": unverified,
	// a label is removed with its delete icon, then Save: nothing typed.
	"iam-labels": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/resource-manager/docs/creating-managing-labels", Verified: true},
	// "confirm you want to delete the bucket and its contents. Click Delete."
	"storage": {Style: DeleteConfirmClick, Source: "https://docs.cloud.google.com/storage/docs/deleting-buckets", Verified: true},
	// UNVERIFIED: no console steps for deleting a batch operations job (docs read 2026-11-19)
	"storage-batch-operations": unverified,
	// UNVERIFIED: "On the Report configuration details page, click Delete." No dialog described (docs read 2026-11-19).
	"storage-inventory-reports": unverified,
	// UNVERIFIED: no console steps for deleting a transfer job read (docs read 2026-11-19)
	"storage-transfer": unverified,
	// UNVERIFIED: no console steps for deleting an agent pool read (docs read 2026-11-19)
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
	// UNVERIFIED: "select the repository to delete. Click Delete." No dialog described (docs re-read 2026-11-19).
	"artifact-registry": unverified,
	// UNVERIFIED: no console steps for deleting a function (docs re-read 2026-11-19)
	"functions": unverified,
	// UNVERIFIED: "Select the trigger and click Delete." No dialog described (docs re-read 2026-11-19).
	"eventarc": unverified,
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
	"bigquery": {Style: DeleteConfirmWord, Source: "https://docs.cloud.google.com/bigquery/docs/managing-datasets", Verified: true},
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
