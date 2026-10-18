# ADR-0006: IAM policy storage without enforcement

- Status: Accepted
- Date: 2026-09-25
- Issue: #308
- **Amended by:** #421 (Cloud KMS), #504 (Cloud Storage), #1233 (Cloud Storage ACLs, 2026-10-15), #1216 (Cloud Run), #1231 (BigQuery), #1367 (Pub/Sub), #1368 (Resource Manager projects, folders and the organization, 2026-10-15), #1304 (Artifact Registry repositories, 2026-10-16), #1486 (Spanner, Bigtable tables, tags and audit configs, 2026-10-16), #1527 (Secure Source Manager instances and repositories, 2026-10-17)
- **Reaffirms:** the no-authorization clause of [ADR-0004](0004-local-access-and-no-authentication.md).
  It supersedes nothing.

## Context

ADR-0004 decided that CloudBurrow performs no authentication or authorization, because *"a
passing IAM test that proves nothing is worse than no IAM test, because it is believed."*
That leaves the IAM methods themselves unserved. Today:

- Cloud Storage bucket IAM answers 404.
- Secret Manager and Cloud Tasks `GetIamPolicy`, `SetIamPolicy` and `TestIamPermissions` return
  UNIMPLEMENTED.
- Pub/Sub IAM is the upstream emulator's, which returns UNIMPLEMENTED.

That breaks code that *manages* policies, even when it never depends on them being enforced.
Terraform's `google_secret_manager_secret_iam_member` is one example; a service that grants
itself access at deploy time is another. Both fail on the first call.

What the tools we are compared against do:

| Tool | IAM behaviour |
|---|---|
| **LocalStack** | Enforces IAM policies when enabled, and names the denied action ([IAM policy enforcement](https://docs.localstack.cloud/aws/capabilities/security-testing/iam-policy-enforcement/)). This is AWS IAM, whose policy language is self-contained. |
| **LocalCloud** | `LOCALCLOUD_IAM_MODE` is `permissive` or `strict` ([configuration](https://local.cloud/docs/configuration/)). |
| **gcp-local** | Round-trips `getIamPolicy`/`setIamPolicy` without enforcing them ([gcp-local](https://github.com/GuitarWag/gcp-local)). |

## Options

### (a) Status quo: the IAM methods are UNIMPLEMENTED

- **What an SDK test can prove:** that the methods return UNIMPLEMENTED. Nothing else.
- **compatibility.md label:** *Unimplemented*.
- **Risk of false confidence:** none. Code that calls these methods fails loudly and locally,
  which is honest but blocks real workflows, such as Terraform modules that grant access
  alongside the resource they create.

### (b) Policy storage only, on Secret Manager and Cloud Tasks

`GetIamPolicy`, `SetIamPolicy` and `TestIamPermissions` on the two services CloudBurrow serves
itself. They carry the real etag semantics and **no enforcement**.

- **What an SDK test can prove:**
  - a policy set through the official client reads back byte-for-byte;
  - a stale `etag` is refused with ABORTED, so read-modify-write loops behave as they do
    against Google;
  - `TestIamPermissions` answers in the documented shape;
  - Terraform `*_iam_member` resources apply and destroy.

  It can **not** prove that a principal is or is not allowed anything, and no test will claim
  to.
- **compatibility.md label:** ***Stored, not enforced***, on every row.
- **Risk of false confidence:** low, and located.
  - A stored policy has no effect, so a developer cannot come to believe one works by
    watching it work.
  - The one method that answers a permission question is `TestIamPermissions`. It returns
    **every** requested permission, which is the literal truth locally (nothing is denied). A
    test asserting that a principal *lacks* a permission fails here rather than passing
    falsely.
  - The label on the rows, and README's "What will not work", say this is not authorization.

### (c) Opt-in enforcement against a pinned snapshot of Google's predefined roles

- **What an SDK test can prove:** that CloudBurrow's evaluator agrees with itself.
- **What it cannot prove:** that the evaluator agrees with Google. GCP authorization also
  depends on:
  - resource-hierarchy inheritance from projects, folders and organizations, none of which
    exist here (ADR-0005, #298);
  - IAM Conditions (CEL on request attributes and time);
  - deny policies and principal access boundaries;
  - organization policy;
  - custom roles;
  - roles that change between snapshots.
- **compatibility.md label:** *Partial* at best, and in practice unlabellable per method,
  because correctness depends on the policy rather than on the RPC.
- **Risk of false confidence:** **high.** A green "permission denied" test here would be
  exactly the believed-but-wrong IAM test ADR-0004 rejected. It would also be believed more,
  because it looks like enforcement.

## Decision

**Accept (b), narrowly. Reject (c). Reaffirm ADR-0004: CloudBurrow authorizes nothing.**

Policy storage is added to exactly these RPCs, each in its own issue:

| Service | RPCs | Follow-up |
|---|---|---|
| Secret Manager (`google.cloud.secretmanager.v1.SecretManagerService`) | `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` on `projects/*/secrets/*` | #365 |
| Cloud Tasks (`google.cloud.tasks.v2.CloudTasks`) | `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` on `projects/*/locations/*/queues/*` | #366 |
| Cloud KMS (the `google.iam.v1.IAMPolicy` mixin on `cloudkms.googleapis.com`) | `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions` on `projects/*/locations/*/keyRings/*` and `projects/*/locations/*/keyRings/*/cryptoKeys/*` | #428, #429, #430, #431 |

Rules both follow-ups must meet:

1. **No enforcement, ever.** No RPC in any service consults a stored policy.
2. `SetIamPolicy` returns the policy with a new `etag`. A request carrying a non-matching
   `policy.etag` is **ABORTED**.
3. **Conditions, audit configs and deny policies are UNIMPLEMENTED**, with the field named.
   They are never stored and ignored. *Amended by #365:* a `version: 3` policy **without**
   conditions is accepted, as Google accepts it. Terraform's `google` provider requests and
   sets version 3 on every `*_iam_member`, conditions or not, so refusing the version refused
   the case this ADR exists for. That was measured: the first `apply` failed with
   `policy.version 3 … notImplemented`. A policy stored here still never holds a condition.
4. `TestIamPermissions` returns every requested permission.
5. Policies follow `--mode`, are cleared by `/admin/reset`, and are captured by `state save`
   where the service is.
6. **Required tests:**
   - an official-SDK compat test of set/get round-trip, stale etag ABORTED, and
     TestIamPermissions;
   - a test that a conditional binding is UNIMPLEMENTED;
   - a Terraform `*_iam_member` apply and destroy through `cloudburrow terraform`, skipped
     without Terraform.
7. Every compatibility.md row for these methods reads ***Stored, not enforced***.

### Cloud KMS (amended by #421)

The decision is unchanged: option (b), no enforcement. Only its scope grows, so this amends
the ADR in place, as rule 3's amendment by #365 does. Google's KMS IAM surface is taken from
[`cloudkms_v1.yaml`](https://github.com/googleapis/googleapis/blob/5b03e5ec0d34ee2c12b7c8431824fa062a7581f6/google/cloud/kms/v1/cloudkms_v1.yaml)
(Y below).

1. **Which resources.** Google serves KMS IAM on key rings, crypto keys, import jobs,
   `ekmConfig` and `ekmConnections` (Y:75-105). CloudBurrow stores policies on **key rings
   and crypto keys only**, the only IAM-bearing resources its KMS creates.
   - On import jobs, `ekmConfig` and `ekmConnections` the three methods are
     **UNIMPLEMENTED** and name the resource type, because CloudBurrow serves none of those
     resources (#396, #397). They never return an empty policy.
   - CryptoKeyVersions have no IAM binding on Google.
2. **Nothing is enforced, Encrypt and Decrypt included.** No KMS RPC reads a stored policy,
   so a `roles/cloudkms.cryptoKeyDecrypter` binding grants nothing and denies nothing.
   CloudBurrow's KMS is not a security boundary (#391).
3. **No inheritance.** On Google a key inherits access from its key ring and project
   ([Hierarchy and inheritance](https://cloud.google.com/kms/docs/iam)). Inheritance matters
   only when a policy is evaluated, and nothing here is evaluated. `GetIamPolicy` on a key
   returns the key's own policy, never one merged with its ring's.
4. **Rule 4, clarified for KMS.** `TestIamPermissions` returns every requested permission on
   a ring or key that exists, as rule 4 says. Rule 4 does not say what happens when the
   resource is missing.
   - For a well-formed ring or key name that does not exist, KMS returns an **empty set**.
     Google documents that `TestIamPermissions` on a missing resource "will return an empty
     set of permissions, not a `NOT_FOUND` error" (Y:59-64).
   - Cloud Tasks answers NOT_FOUND there (#366). The difference is deliberate: this project
     builds to Google's spec ([ADR-0005](0005-kubernetes-foundation-and-upstream-reuse.md)).
   - The behaviour is documented by Google but not measured, so its test carries an
     `// unverified:` annotation (#384).
5. **Rule 5 for KMS.** A policy is stored on its ring or key record, so it has that record's
   persistence and is cleared by `/admin/reset`, including `reset?project=` (#387).
   - Cloud KMS is not captured by `state save` (#388), so its policies are not captured
     either.
   - Terraform never deletes a ring, and destroying a `google_kms_crypto_key` destroys its
     versions only (hashicorp/terraform-provider-google@7a2398d366,
     `resource_kms_key_ring.go:346-353`, `resource_kms_crypto_key.go:626-665`).
     `DeleteCryptoKey` stays UNIMPLEMENTED here (#424), so a policy lasts until a reset.
6. **Rule 6 for KMS.** The required tests are:
   - an official-SDK compat test over gRPC **and** over REST;
   - a Terraform `google_kms_key_ring_iam_member`, `google_kms_crypto_key_iam_member` and
     `google_kms_crypto_key_iam_binding` apply, clean plan and destroy through
     `cloudburrow terraform`;
   - a real-gcloud `kms keyrings add-iam-policy-binding` and `kms keys
     add-iam-policy-binding` test through `gcloud-setup`.

### Cloud Storage (amended by #504)

The decision is unchanged: option (b), no enforcement. CloudBurrow now builds its own Cloud
Storage server (#485), so bucket IAM is no longer the upstream emulator's to leave alone, and
this amends the ADR in place as #421 did.

1. **Which methods.** `storage.buckets.getIamPolicy` (with `optionsRequestedPolicyVersion`),
   `setIamPolicy` and `testIamPermissions`, on `b/{bucket}/iam`, on the builtin server only.
   Object IAM, managed-folder IAM and every ACL method (bucket, object and default object)
   stay **501 naming the method**; they are never an empty policy or list. fake-gcs-server
   serves none of them, and its calls keep failing until the cut-over (#519).
   *Amended by #828:* managed-folder IAM is built. *Amended 2026-10-15 by #1233:* the ACL
   methods are built, stored and not enforced (below). Object IAM stays 501.
2. **Rules 1-5 apply.**
   - Nothing reads a stored policy (rule 1).
   - A set returns a new `etag`, and a set carrying a stale one is refused (rule 2). The
     JSON API has no ABORTED; its code for a failed precondition is **412
     `conditionNotMet`**, which no page states for this method, so the test carries an
     `// unverified:` annotation.
   - A condition is 501 naming the field, and a version 3 policy without conditions is
     accepted (rule 3, as amended by #365).
   - `testIamPermissions` returns every requested permission on a bucket that exists (rule
     4). On a missing bucket it is 404, which is UNVERIFIED (docs re-read 2026-10-17: <https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/testIamPermissions>).
   - The policy is part of the bucket record, so it has the server's persistence, and reset
     (#510) and state capture (#511) take it with the bucket (rule 5).
3. **No inheritance.** A policy is the bucket's own; project-level bindings do not exist
   here, so a new bucket's policy has no bindings, where Google's shows the project's
   convenience bindings.
4. Every compatibility.md row for these methods reads ***Stored, not enforced***.

### Cloud Storage ACLs (amended 2026-10-15 by #1233)

The maintainer decided on 2026-10-15 that Cloud Storage's access control lists are
**stored and returned, not enforced**, reversing the #504 amendment's "every ACL method stays
501". The decision is still option (b): CloudBurrow does no authentication (ADR-0004), so it
has no caller an ACL could allow or refuse, and an ACL is data like a policy. This amends the
ADR in place, as #421 and #504 did.

1. **Which methods.** `bucketAccessControls`, `defaultObjectAccessControls` and
   `objectAccessControls`: `get`, `list`, `insert`, `patch`, `update` and `delete`; the
   `acl` and `defaultObjectAcl` fields of a bucket and the `acl` field of an object, returned
   under `projection=full`; `predefinedAcl` and `predefinedDefaultObjectAcl` on bucket insert,
   patch and update, and `predefinedAcl` on object insert, patch and update.
2. **As Google documents them.** Entities are `user-`, `group-`, `domain-`,
   `project-<team>-<number>` (a project ID is stored as its number, `resource.ProjectNumber`,
   #1211), `allUsers` and `allAuthenticatedUsers`; roles are OWNER, READER and, on a bucket's
   own ACL only, WRITER. A new bucket's ACLs are `projectPrivate`'s, and a new object takes its
   bucket's default object ACL. Under uniform bucket-level access every ACL method fails
   **400**, and a full projection carries empty ACL lists. The 400's message, and the messages
   of invalid entities and roles, are UNVERIFIED (docs re-read 2026-10-17: <https://docs.cloud.google.com/storage/docs/access-control/lists>).
3. **No identity.** CloudBurrow has no caller, so the "object owner" that Google's predefined
   object ACLs grant OWNER is the bucket's project owners, `project-owners-<number>`.
4. **Rules 1 and 5 apply.** Nothing reads a stored ACL, and ACLs live on the bucket and object
   records, so they follow persistence, reset and state capture. Each ACL change advances the
   metageneration of the bucket or object it belongs to, as Google's does.
5. Every compatibility.md row for these methods reads **Verified**, *stored, not enforced*.

### Cloud Run (amended by #1216)

The decision is unchanged: option (b), no enforcement. The closing paragraph of this
decision said Cloud Run's adapter "may follow the same pattern later, under its own issue"; #1216 is that issue, and
this amends the ADR in place as #421 and #504 did.

1. **Which methods.** `GetIamPolicy`, `SetIamPolicy` and `TestIamPermissions` of
   `google.cloud.run.v2.Services` on `projects/*/locations/*/services/*`, and of
   `google.cloud.run.v2.Jobs` on `projects/*/locations/*/jobs/*`. WorkerPools are not served,
   so theirs are not either.
2. **Rules 1-5 apply.** The shared `internal/iampolicy` gives the same etag, version and
   condition handling as Secret Manager's. `TestIamPermissions` returns every requested
   permission on a resource that exists and, as KMS does (its rule 4), an empty set on a
   well-formed name that does not, which `iam_policy.proto` documents; that case carries an
   `// unverified:` annotation.
3. **Nothing is enforced, the service URL included.** A `roles/run.invoker` binding, or the
   absence of `allUsers`, does not close a service's URL: every caller reaches it, as every
   caller already did. `RunJob` reads no policy.
4. **Where it is kept.** In a ConfigMap of its own beside the resource, owned by the Knative
   Service or the job's ConfigMap, so the cluster deletes it with the resource. Kept on the
   resource's own object it would change the resource's `etag` and make the caller's next
   update ABORTED. Cloud Run is neither reset nor captured by `state save`, so neither is
   its policy.
5. **Required tests:** official-SDK compat tests of each resource's policy, and
   `TestTerraformRunIAM` (#1236): `google_cloud_run_v2_service_iam_*` and
   `google_cloud_run_v2_job_iam_*` member, binding and policy through `cloudburrow terraform`.
6. Every compatibility.md row for these methods reads ***Stored, not enforced***.

~~**Project IAM stays UNIMPLEMENTED.**~~ Superseded by the Resource Manager amendment below
(#1368, 2026-10-15): a project's policy is now stored, and still inherited by nothing.

### BigQuery (amended by #1231)

**Decided by the maintainer on 2026-10-15** (#1231): BigQuery table, routine and row access
policy IAM are stored and returned, **not enforced**, as on Secret Manager, Cloud Tasks, Cloud
KMS, Cloud Storage and Cloud Run (#1216). The decision is unchanged: option (b), no
enforcement. #1218 left these methods 501 because this ADR did not name BigQuery; this amends
it in place, as #421 and #504 did.

1. **Which methods.** `bigquery.tables.getIamPolicy`, `setIamPolicy` and `testIamPermissions`
   (a view is a table), `bigquery.routines.getIamPolicy`, `setIamPolicy` and
   `testIamPermissions`, and `bigquery.rowAccessPolicies.getIamPolicy` and
   `testIamPermissions`, all answered by the BigQuery front (`internal/bigqueryfront/iam.go`),
   since the emulator behind it has none of them. Dataset access stays the dataset's `access`
   list, which `datasets.patch` already stores.
2. **Rules 1-5 apply to tables and routines.** The shared `internal/iampolicy` gives the
   same etag, version and condition handling as Secret Manager's: a set returns a new
   `etag`; a stale one is refused **409** with `ABORTED`'s mapping, the JSON API's code for it
   being unmeasured, which the test and its compatibility.md row say (tools/coverage's `// unverified:`
   annotation has no form for a BigQuery REST method); a condition or audit
   config is 501 naming the field; a version 3 policy without conditions is accepted.
   `testIamPermissions` returns every requested permission on a resource that exists and, as
   KMS does (its rule 4), an empty set on a well-formed name that does not, which
   `iam_policy.proto` documents, and BigQuery's own `tables.testIamPermissions` reference says the same.
3. **A row access policy's IAM policy is its grantees.** The API has no
   `rowAccessPolicies.setIamPolicy`: `RowAccessPolicy.grantees` are the policy's members,
   holding `roles/bigquery.filteredDataViewer`, and `rowAccessPolicies.update` changes them.
   `getIamPolicy` answers that one binding from the grantees the front already keeps
   (#1218). This is the one policy here that decides something: the front filters a table's
   rows by its grantees, against the one identity it takes every caller to be (#1218). That
   is row filtering, documented with the row access policies, not authorization; nothing
   refuses a caller.
4. **Nothing is enforced on tables and routines.** No query, read, write or routine call
   consults a table's or a routine's policy.
5. **Where it is kept, and for how long.** In the front's state directory
   (`iampolicies.json`), beside the row access policies, so a restart of the front keeps it.
   It is kept with the `creationTime` of the resource it was set on, and goes with the
   resource: a `tables.delete` or `routines.delete` through the front drops it, and a
   resource found gone or made again (a new `creationTime`) has none, as on Google. An
   emulator restart, which empties BigQuery, drops every policy. BigQuery is not captured by
   `state save`, so neither are its policies.
6. **Required tests:** official-SDK compat tests (`Table.IAM` of the Go client, and the
   generated client for routines and row access policies), the console's Permissions tab,
   and `TestTerraformBigQueryIAM` (#1236): `google_bigquery_table_iam_member`, `_binding` and
   `_policy` on a table and a view, and `google_bigquery_routine_iam_*` on a routine, through
   `cloudburrow terraform`, which sets `big_query_custom_endpoint` for it.
7. Every compatibility.md row for these methods reads ***Stored, not enforced***.

### Pub/Sub (amended by #1367)

Pub/Sub's topic, subscription, snapshot and schema IAM are stored and returned, **not
enforced**, as on Secret Manager, Cloud Tasks, Cloud KMS, Cloud Storage, Cloud Run and BigQuery
(#1231), following the maintainer's "store, not enforce" decisions for BigQuery IAM (#1231) and
Cloud Storage ACLs (#1233). The decision is unchanged: option (b), no enforcement. The emulator
serves no IAM (over REST its answer was a plain-text 404), so CloudBurrow's Pub/Sub front
(`internal/pubsubfront/iam.go`) answers `google.iam.v1.IAMPolicy` `GetIamPolicy`, `SetIamPolicy`
and `TestIamPermissions` on `projects/*/topics/*`, `projects/*/subscriptions/*`,
`projects/*/snapshots/*` and `projects/*/schemas/*`, over gRPC and REST, through the shared
`internal/iampolicy`, with Secret Manager's etag, version and condition handling (rules 1-3). On
a resource the emulator does not have, `GetIamPolicy` and `SetIamPolicy` are NOT_FOUND and
`TestIamPermissions` returns no permissions, as iam_policy.proto documents (#1595). It
returns, of the permissions asked, those Pub/Sub defines on that kind of resource; nothing is
denied, and a permission Pub/Sub does not define there is not one a caller could hold on it.
A policy is kept in the front's state file, and goes with its resource: a delete, an expiry or a
create of the same name through the front drops it, so `/admin/reset`, which deletes Pub/Sub's
resources through the front, clears them (rule 5). Every compatibility.md row for these methods
reads ***Stored, not enforced***.

### Resource Manager projects, folders and the organization (amended by #1368)

**Decided by the maintainer on 2026-10-15** (#1368): a project's IAM policy is **stored and
never enforced**, as every other policy here is. This reverses the refusal above, which held that
a stored project policy would be read as the protection it is not; the maintainer chose to store
it and say so, as for BigQuery (#1231) and Cloud Storage ACLs (#1233). The decision is unchanged:
option (b), no enforcement. Folders and the local organization, which CloudBurrow serves since
#1294, are stored the same way.

1. **Which methods.** `GetIamPolicy`, `SetIamPolicy` and `TestIamPermissions` on
   `projects/{id}` or `projects/{number}`, `folders/{number}` and the local organization, over
   gRPC and the v3 REST paths, v1 REST `projects.*` and `organizations.*` (which `gcloud
   projects` and Terraform's `google_project_iam_*` call) and v2 REST `folders.*` (`gcloud
   resource-manager folders`), served by `internal/service/resourcemanager/iam.go`.
2. **Rules 1-3 apply.** The shared `internal/iampolicy` gives Secret Manager's etag, version and
   condition handling: a set returns a new `etag`, a stale one is ABORTED (409 over REST), a
   condition or audit config is UNIMPLEMENTED naming the field, and a version 3 policy without
   conditions is accepted. An `auditConfigs` path in the update mask with no audit configs in the
   policy is a no-op, not a refusal: Terraform's project IAM sends `bindings,etag,auditConfigs` on
   every set.
3. **No inheritance.** A project's, a folder's or the organization's bindings apply to nothing.
   A resource's own `getIamPolicy` (a bucket's, a topic's, a secret's) still shows only its own
   bindings, a new bucket's still shows none, and a project's policy is not merged into a
   folder's or the reverse. The policy is a record of what was set, read back as it was set.
4. **`TestIamPermissions`** answers, of the permissions asked, those that can be held on that
   resource: every well-formed `service.resource.verb` permission, since any service's
   permission can be granted on a project, less a folder's or an organization's
   (`resourcemanager.folders.*`, `resourcemanager.organizations.*`) asked of a project and an
   organization's asked of a folder. Nothing is denied, as Pub/Sub's rule says.
5. **Where it is kept, and for how long.** In the project registry's store
   (`resourcemanager/iam/…`), so it has the registry's persistence and `state save` captures it.
   A project or folder pending deletion keeps its policy and refuses a set
   (FAILED_PRECONDITION); purged after the 30 days, it loses it, and a project made again under
   that ID starts with none. `/admin/reset` clears every stored policy (component
   `resourcemanager`; `reset?project=` clears that project's), and leaves the projects, folders
   and organization, which are a registry.
6. **Required tests:** official-SDK compat tests with `cloud.google.com/go/resourcemanager/apiv3`
   `ProjectsClient`, `FoldersClient` and `OrganizationsClient` and with
   `google.golang.org/api/cloudresourcemanager/v1`; `gcloud projects add-iam-policy-binding`,
   `get-iam-policy` and `remove-iam-policy-binding`; and `TestTerraformProjectIAM`:
   `google_project_iam_member`, `_binding` and `_policy` through `cloudburrow terraform`. The
   console's IAM page (IAM & Admin) shows the selected project's policy with Grant access and
   Remove principal, and says it is stored, not enforced and inherited by nothing.
7. Every compatibility.md row for these methods reads ***Stored, not enforced***.

### Artifact Registry repositories (amended by #1304)

`ArtifactRegistry.GetIamPolicy`, `SetIamPolicy` and `TestIamPermissions` on
`projects/*/locations/*/repositories/*` store a policy as Cloud Run's do, with the same rules:
nothing enforces it, a stale `etag` is ABORTED, conditions are UNIMPLEMENTED, and
`TestIamPermissions` grants every permission on a repository that exists. The policy is kept
beside the repository, saved with it in the registry's state directory, and removed with it, so a
repository made again under the same name starts with none; a policy change leaves the
repository alone. Required test: `TestTerraformArtifactRegistryRepositoryIAM`
(`google_artifact_registry_repository_iam_member`, `_binding` and `_policy`, read back with the
official `artifactregistry` client).

### Spanner, Bigtable tables, tags and audit configs (amended by #1486)

The decision is unchanged: option (b), no enforcement. #1304's Terraform batches found these IAM
methods still refused where every other service stores the policy; the maintainer's #1486 stores
them, and so reverses rule 3 for audit configs on the resources Google keeps them on.

1. **Which methods.** `InstanceAdmin` and `DatabaseAdmin` `GetIamPolicy`, `SetIamPolicy` and
   `TestIamPermissions` on Spanner instances and databases, answered by the call front in the
   Spanner pod (`internal/spannerfront/iam.go`), over gRPC and REST, since the emulator refuses
   them; `BigtableTableAdmin`'s on tables, answered by the Bigtable front
   (`internal/bigtablefront/tableiam.go`), over gRPC and REST; and `TagKeys` and `TagValues`' on
   tag keys and values (`internal/service/resourcemanager/iam.go`). A Spanner backup's or database
   role's, and a Bigtable authorized view's or backup's, stay **UNIMPLEMENTED**: the emulators keep
   none of those resources.
2. **Rules 1-4 apply.** The shared `internal/iampolicy`: a new etag on every set, a stale one
   ABORTED, conditions UNIMPLEMENTED, version 3 accepted. `TestIamPermissions` answers every
   permission asked on a Spanner or Bigtable resource that exists (NOT_FOUND on one that does not,
   as Spanner documents), and on a tag the tag permissions alone (UNVERIFIED (docs re-read 2026-10-17: <https://docs.cloud.google.com/resource-manager/reference/rest/v3/tagValues/testIamPermissions>)).
3. **Audit configs, rule 3 amended.** On projects, folders and the organization (#1368's
   resources) a policy's `audit_configs` are **stored and returned, not acted on**: no audit log
   is written or withheld because of one. They change only as `update_mask` says
   (`iam_policy.proto`: only the fields in the mask change, and no mask means
   `bindings, etag`), so a set whose mask leaves them out keeps them, and Terraform's
   `bindings,etag,auditConfigs` with none clears them. Every other resource still refuses them
   UNIMPLEMENTED, naming the field.
4. **Where it is kept (rule 5).** Spanner's and Bigtable's in their fronts' memory, as the
   emulators keep their data, and gone with the resource: DropDatabase, DeleteInstance and
   DeleteTable through the front, and a Bigtable instance's deletion, drop them, so a resource made
   again starts with none. A tag's in the registry's store beside it, with the registry's
   persistence, `state save` and `/admin/reset`, and dropped when the tag is deleted.
5. **Required tests:** official-SDK compat tests (`TestSpannerIAM`, `TestBigtableTableIAM`,
   `TestResourceManagerTagIAM`, `TestResourceManagerAuditConfigs`), Terraform
   (`TestTerraformSpannerIAM`, `TestTerraformBigtableTableIAM`, `TestTerraformTagIAM`,
   `TestTerraformProjectIAMAuditConfig`), gcloud `add-iam-policy-binding` for each resource, and
   the console's Permissions tab on a Spanner instance's and database's, a Bigtable table's and a
   tag key's and value's page.
6. Every compatibility.md row for these methods reads ***Stored, not enforced***.

### Secure Source Manager instances and repositories (amended by #1527)

`SecureSourceManager.GetIamPolicyRepo`, `SetIamPolicyRepo` and `TestIamPermissionsRepo` on
`projects/*/locations/*/repositories/*`, and the `google.iam.v1.IAMPolicy` mixin on
`projects/*/locations/*/instances/*`, store a policy as Artifact Registry's do, with the same
rules: nothing enforces it, a stale `etag` is ABORTED, conditions are UNIMPLEMENTED, and the test
methods grant every permission on a resource that exists. #1521 served the instances and
repositories, and Terraform's `google_secure_source_manager_instance_iam_*` and
`_repository_iam_*`, and gcloud source-manager's IAM commands, otherwise failed on the first
call. The instance mixin is served on the JSON API only (`/securesourcemanager/v1/…` on the Run
port), since the Run port's gRPC `IAMPolicy` service is Cloud Functions'. A policy is saved with
the instances and repositories in their state file and removed with its resource, so one made
again under the same name starts with none. Required tests: `TestSecureSourceManagerIAM` (the
official client, gRPC and REST), `TestTerraformSecureSourceManager` (`_repository_iam_member`,
`_instance_iam_member`), and the console's Permissions tab on an instance's and a repository's
page. Every compatibility.md row for these methods reads ***Stored, not enforced***.

Every service other than these nine (Secret Manager, Cloud Tasks, Cloud KMS, Cloud
Storage's buckets and their ACLs, Cloud Run's services and jobs, BigQuery's tables, routines and row
access policies, Pub/Sub's topics, subscriptions, snapshots and schemas, Resource Manager's
projects, folders and organization, and Artifact Registry's repositories), and Spanner's instances and databases, Bigtable's
tables and Resource Manager's tag keys and values (#1486), and Secure Source Manager's instances and
repositories (#1527), keeps its current behaviour.

## Consequences

- Terraform modules and deploy scripts that grant access alongside a resource work locally.
  **None of them is tested for whether the grant is right**, and the docs say so.
- README's "What will not work" changes from "No IAM, anywhere" to: *no IAM
  **enforcement**, anywhere; Secret Manager, Cloud Tasks and, once #428 lands, Cloud KMS store
  policies without enforcing them.* The instruction not to test permissions here stands
  unchanged. README and status.md change when each service's storage lands, not before, so
  no page claims untested support.
- A future request for enforcement has to supersede this ADR and ADR-0004's clause, and answer
  option (c)'s objection: an evaluator whose disagreements with Google cannot be detected.
