# ADR-0006: IAM policy storage without enforcement

- Status: Accepted
- Date: 2026-09-25
- Issue: #308
- **Amended by:** #421 (Cloud KMS)
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

Every service other than these three (Secret Manager, Cloud Tasks and Cloud KMS) keeps its current behaviour, Storage, Pub/Sub and Cloud Run included. Cloud
Storage's and Pub/Sub's IAM belong to the upstream emulators and are not interposed. Cloud
Run's adapter may follow the same pattern later, under its own issue.

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
