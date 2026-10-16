# Backup and DR

CloudBurrow serves the Backup and DR Service API v1 (`backupdr.googleapis.com`, #1433) as far as
Cloud SQL's integration uses it: backup vaults, backup plans with their rules, backup plan
associations that enroll a Cloud SQL instance, the data sources they make in the plan's vault,
and the backups taken of them. Cloud SQL restores those backups, and recovers a data source's
instance to a point in time (`instances.pointInTimeRestore`, #1425).

It is opt-in, beside a Cloud SQL service:

```sh
cloudburrow up --services cloudsql,cloudsql-mysql,storage,backupdr
```

It runs in the CLI process on `--port-backupdr` (default `9024`, moved by `--port-base`), bound
only when `backupdr` and `cloudsql` or `cloudsql-mysql` are enabled. `cloudburrow env` exports
`CLOUDBURROW_BACKUPDR_ENDPOINT` and `CLOUDSDK_API_ENDPOINT_OVERRIDES_BACKUPDR`, `cloudburrow
gcloud-setup` writes the `backupdr` override for `gcloud backup-dr`, and `cloudburrow terraform`
sets the provider's `backup_dr_custom_endpoint`. No client library reads the variable, so pass
`http://<it>/` to `option.WithEndpoint` of `google.golang.org/api/backupdr/v1`:

```go
svc, _ := backupdr.NewService(ctx,
    option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_BACKUPDR_ENDPOINT")+"/"),
    option.WithoutAuthentication())
```

Per-method status is generated in [coverage/backupdr.md](coverage/backupdr.md); the claims and
their tests are in [compatibility.md](compatibility.md#backup-and-dr-service--backupdrgoogleapiscom-v1).

## What is served

Everything is under `projects/{project}/locations/{location}`; `locations.list` and `get` answer
the regions CloudBurrow lists, and `-` lists every location's vaults, plans and associations.

| Resource | Methods | What it does |
|---|---|---|
| `backupVaults` | `create`, `get`, `list`, `delete` | `backupMinimumEnforcedRetentionDuration` is required (`"86400s"`), `accessRestriction` defaults to `WITHIN_ORGANIZATION`. `backupCount` and `deletable` are read from what the vault holds. Deleting a vault a plan uses is FAILED_PRECONDITION; one holding data sources needs `force=true`, which deletes their backups too. `allowMissing=true` makes a missing vault a done delete. |
| `backupPlans` | `create`, `get`, `list`, `delete` | `resourceType` must be `sqladmin.googleapis.com/Instance`; `backupVault` must exist; at least one rule, each with a unique `ruleId`, a positive `backupRetentionDays` and a `standardSchedule` (`recurrenceType` HOURLY, DAILY, WEEKLY, MONTHLY or YEARLY, an IANA `timeZone`, `hourlyFrequency` for HOURLY, an optional `backupWindow`). `logRetentionDays` is kept and reported. A plan an association applies cannot be deleted. |
| `backupPlanAssociations` | `create`, `get`, `list`, `delete`, `triggerBackup` | `resource` is `projects/{project}/instances/{instance}` (with or without `//sqladmin.googleapis.com/`), `resourceType` `sqladmin.googleapis.com/Instance`; the instance must exist. An instance is protected by one association at a time. Creating it makes a data source in the plan's vault (or reuses the one an earlier association left there); `rulesConfigInfo` reports each rule's last backup. Deleting it stops protection; the data source and its backups stay. `triggerBackup` with a `ruleId` takes an on-demand backup, kept for the rule's retention or `customRetentionDays`. |
| `backupVaults.dataSources` | `get`, `list` | The instance protected (`dataSourceGcpResource.cloudSqlInstanceDatasourceProperties`: name, version, tier, create time), `backupCount`, and `backupConfigInfo` (last backup state and consistency time, the plan and association). `configState` is ACTIVE while an association applies, PASSIVE after. |
| `backupVaults.dataSources.backups` | `get`, `list`, `delete` | `state` CREATING, ACTIVE or ERROR from the Cloud SQL backup underneath, `backupType` SCHEDULED or ON_DEMAND, `consistencyTime` when it finished, `expireTime` from the retention, `cloudSqlInstanceBackupProperties` and `gcpBackupPlanInfo`. `-` as the data source lists a vault's every backup. |
| Updates (#1462) | `backupVaults.patch`, `backupPlans.patch`, `backupPlanAssociations.patch`, `dataSources.patch`, `backups.patch` | `updateMask` is required (camelCase or snake_case); a field outside these is 400. A vault: description, labels, `backupMinimumEnforcedRetentionDuration`, `accessRestriction`, `effectiveTime`, with `validateOnly`. A plan: description, labels, `backupRules`, `logRetentionDays`, checked as on create. An association: `backupPlan` only, as Google documents; a plan in another vault gives the instance a data source there, and the old one goes PASSIVE. A data source: labels (`allowMissing` is refused, since data sources are made by associations). A backup: `expireTime`, `enforcedRetentionEndTime` (extended, never reduced; `expireTime` cannot come before it), labels, description. |
| `backupPlans.revisions` | `get`, `list` | Each create and patch of a plan is a revision, `v0`, `v1`, …, newest first: the current one ACTIVE, the others INACTIVE, each with its `backupPlanSnapshot`. The plan reports `revisionId` and `revisionName`; associations and data sources name the current one, and each backup the one it was taken under. |
| Views (#1462) | `backupVaults.fetchUsable`, `backupVaults.testIamPermissions`, `backupPlanAssociations.fetchForResourceType`, `backups.fetchForResourceType`, `dataSourceReferences.get`, `list`, `fetchForResourceType` | Over what the service holds; a resource type other than `sqladmin.googleapis.com/Instance` lists nothing. `testIamPermissions` returns every permission asked (IAM is not enforced). A data source reference is `{location}/dataSourceReferences/{data source id}`, the project-level view of the data source. |
| `operations` | `get`, `list`, `cancel`, `delete` | `cancel` sets `requestedCancellation` on a running operation (a backup already being dumped still finishes); `delete` forgets the record. Every mutation returns a `google.longrunning.Operation` with `OperationMetadata`: done at once, with the resource as its response, except `triggerBackup`, whose operation runs until the backup is taken (or fails, with the error). Operations are kept in memory. |

The records are kept in the instance directory (`backupdr.json`) for a persistent instance, and
in memory for an ephemeral one.

## Backups are real Cloud SQL backups

A Backup and DR backup is a Cloud SQL backup run of the instance (#1330, #1380): each
application database dumped with the engine's own tools, with a point-in-time recovery base. The
Cloud SQL Admin API keeps it marked as the Backup and DR Service's, so:

- `backupRuns.list` and `get` do not show it, as Cloud SQL lists only its own backups;
- deleting the instance does not delete it, as a vault's backup outlives its instance;
- deleting the Backup and DR backup, or its expiry, deletes its dump.

**Scheduled backups.** Once a minute each association's rules are checked: a backup is due when
the time, in the rule's `timeZone`, is inside its `backupWindow` (all day without one), on a day
the schedule names (`daysOfWeek` for WEEKLY, `daysOfMonth` for MONTHLY, `months` and
`daysOfMonth` for YEARLY), and the rule has taken none since this window opened (for HOURLY,
none in the last `hourlyFrequency` hours). A backup whose time passes while CloudBurrow is not
running is taken at the next check inside its window, not afterwards. Expired backups are
deleted by the same check.

## Restoring

Cloud SQL restores its Backup and DR backups, as it does at Google:

- **A backup into an instance:** `instances.restoreBackup` on the target instance with
  `backupdrBackup` set to the backup's name
  (`projects/…/locations/…/backupVaults/…/dataSources/…/backups/…`), instead of
  `restoreBackupContext` (giving both is 400). The target's databases are dropped and the
  backup's loaded, as any restore; the source instance may since have been deleted.
- **A point in time into a new instance:** `instances.pointInTimeRestore` (gcloud `sql
  instances point-in-time-restore DATASOURCE TARGET POINT_IN_TIME`) with the data source. The
  request is checked as Google checks it (#1425), the data source is resolved to the instance it
  protects, and the target is created by the clone-to-a-time path (#1330): the instance's
  settings, recovered from its newest base before that time and its log. The instance needs
  point-in-time recovery (PostgreSQL) or the binary log (MySQL) on, and the time must be in its
  recovery window, as for `instances.clone` with `pointInTime`.

Without `--services backupdr`, both answer 501 saying the Backup and DR Service is not served.

## What is refused

Each answers 501 UNIMPLEMENTED with the reason:

- **Other workload types.** A plan or association for any `resourceType` but
  `sqladmin.googleapis.com/Instance` (Compute Engine instances, disks, AlloyDB, Filestore) needs a
  server of that type, which CloudBurrow does not run.
- **`backups.restore`.** Its targets are Compute Engine instances and disks; the answer names
  `instances.restoreBackup` with `backupdrBackup` and `instances.pointInTimeRestore`, which restore
  a Cloud SQL backup.
- **`pointInTimeRestore` into another project** than the data source's instance's: call it with
  that project as the parent.
- **`encryptionConfig`** on a vault: it needs a Cloud KMS key; backups are not encrypted with a
  customer key.
- **Management servers** (`managementServers.*`): a management server is a Google-managed backup
  and recovery appliance VM on Compute Engine, reached over VPC peering.
- **The appliance protocol** (`dataSources.initiateBackup`, `finalizeBackup`, `abandonBackup`,
  `fetchAccessToken`, `setInternalStatus`, `remove`): what an appliance's agents call.
- **Trials** (`getTrial`, `trial.subscribe`, `trial.end`): billing-account state, and CloudBurrow
  has no billing accounts.
- **Resource backup configs** (`resourceBackupConfigs.list` and `fetch`, and the folder and
  organization `fetch`): they inventory a project's (or every project's) resources through Cloud
  Asset Inventory.
- **`serviceConfig.initialize`**: it creates Google's default vault and plan, whose names, rules
  and retention Google does not document; create them with `backupVaults.create` and
  `backupPlans.create`.

IAM is not enforced on any of it, as elsewhere (ADR-0004).

## gcloud and Terraform

`gcloud backup-dr backup-vaults`, `backup-plans`, `backup-plan-associations`, `data-sources` and
`backups` reach the server through the configuration `gcloud-setup` writes (`TestGcloudBackupDR`).
The Terraform provider's `google_backup_dr_backup_vault`, `google_backup_dr_backup_plan` and
`google_backup_dr_backup_plan_association` apply, plan clean and destroy through
`backup_dr_custom_endpoint` (`TestTerraformBackupDR`).

## The console

The console's **Backup and DR** page lists vaults, plans, associations, data sources and backups,
creates and deletes them, takes an on-demand backup, and restores a backup into an instance or
a data source to a point in time.
