# Changelog

All notable changes to CloudBurrow are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); before 1.0.0, a minor
version may change behaviour.

The release workflow publishes a tag's section of this file as the release notes, and refuses
to publish a tag that has no section here. Before tagging `vX.Y.Z`, move the entries under
**Unreleased** into a new `## [X.Y.Z] - YYYY-MM-DD` section and add its link at the bottom.

## [Unreleased]

### Added

- **A release is built only from a commit CI has passed** (#596): the release workflow's first
  job requires a successful `ci-green` check for the tagged SHA, waiting up to 45 minutes while
  CI for it is still running, and fails with the SHA named before anything is built, pushed or
  published when there is none. [docs/install.md](docs/install.md#install-a-release) describes
  it.
- **The macOS binaries can be signed and notarized** (#605): the release workflow signs both
  darwin binaries with a Developer ID certificate (hardened runtime, secure timestamp) and
  notarizes them before the checksums, formula and attestations are computed. It is active
  only once the maintainer adds the Apple secrets; until then releases stay unsigned, and the
  release notes say which. [docs/install.md](docs/install.md#gatekeeper) explains how to
  verify, and what to do with an unsigned binary.
- **The local-AI runtime image is published with each release** (#602):
  `ghcr.io/cloudburrow/litert-lm:<tag>`, linux/amd64 and linux/arm64, attested, its digest in
  the release notes. A release CLI defaults `--local-ai-image` to that digest, so local
  generation no longer needs a checkout and `make litert-lm`.
- **CloudBurrow's own Cloud Storage server**, built to the API rather than reused (ADR-0005
  amendment, #486), and now the only Cloud Storage backend (#519). It serves buckets and
  objects over JSON, uploads including resumable and XML multipart, ranged reads, every
  precondition, `objects.list` with prefixes, offsets, globs and pages, compose, copy, rewrite
  and move, the batch endpoint, object versioning, soft delete and restore, retention policies,
  locks and holds, lifecycle rules, CORS, HMAC keys, notifications to Pub/Sub, the XML API
  subset, and signed URLs verified and failing closed (#489–#509). Bucket IAM policies are
  stored and never enforced (#504). It runs as a locally built image in one in-cluster
  Deployment (#514), with reset and seed (#510), state snapshots (#511), persistence by
  `--mode` (#512), events, metrics and faults (#513), and the console, status and request log
  (#518).
- Cloud Storage verified through the official Python client (#516), `gcloud storage` and
  `gsutil` (#517) and Terraform (#515), and compared against Google's `storage-testbench` by a
  differential oracle (#497).
- Cloud KMS through `cloudburrow terraform` (#425) and `gcloud kms` (#426, #427), and IAM
  policies stored on key rings and keys, never enforced, over gRPC and REST (#428–#431).
- A Cloud KMS oracle comparing the resource RPCs and the version lifecycle against a pinned
  `fakekms`, run nightly (#419, #420).
- A release smoke test: after publishing, the release workflow installs the tag with the
  installer (checksum and attestation verified) on Linux and macOS, and installs, tests and
  strictly audits the Homebrew formula (#603).
- This changelog, and [SECURITY.md](SECURITY.md) with how to report a vulnerability (#603).
- RSA signed URLs verify on an `up` instance: the ADC fixture's key is registered with the
  storage server, and `storage.signingCerts` adds others (#577).

### Changed

- `--mode ephemeral` now also applies to Cloud KMS (#481) and Secret Manager (#483) state.
- `up` creates the managed namespace whenever the cluster is up, not only with a backend
  (#571).
- A Cloud Run revision that fails at startup is reported after 240s rather than Knative's 600s
  (#568).
- Secret Manager refuses the create fields it would drop (ttl, rotation, topics and others),
  refuses list filters, compares request etags, and verifies and returns payload CRC32C (#580).
- Cloud Tasks refuses OIDC and OAuth tokens, logging config and list filters it would drop,
  keeps `dispatch_deadline` and honours `max_retry_duration` (#578).
- The embedded storage servers are no longer committed; `make build` and releases build them
  (#585).
- Release notes are taken from this changelog instead of fixed text (#603).
- The rendered Homebrew formula no longer sets a `version` that repeats the one in its URLs
  (#603).

### Fixed

- Port forwarding replaces a dead tunnel on evidence rather than on one client's failure
  (#526), retries a launch that does not carry (#572), and notices a pod that restarted its
  containers (#566).

### Security

- Every `/admin` route now requires a per-instance token (#553).

## [0.1.0] - 2026-09-25

The first release: a local Google Cloud emulator that runs in a local Kubernetes cluster and is
driven through the official Google Cloud SDKs. What is and is not supported is recorded per
operation in [docs/compatibility.md](docs/compatibility.md) and summarised in
[docs/status.md](docs/status.md).

### Added

- **A CLI and an owned local Kubernetes cluster**: `up`, `status`, `stop`, `reset`, `delete`,
  `env`, `doctor`, `logs`, `wait`, `up --detach`, `status --format json`, `diagnose`,
  `gcloud-setup` and `gcloud-teardown`, and `cloudburrow terraform` (#53, #54, #89, #293, #305,
  #325, #326, #327, #331).
- **Services started by default**: Cloud Storage, with notifications to Pub/Sub (#95); Pub/Sub,
  on Google's emulator; Cloud Tasks with HTTP dispatch and retry (#66, #67, #322); Cloud Run v2
  backed by Knative Serving, including revisions and updates (#68, #299, #300); and Secret
  Manager over gRPC and JSON, backed by Kubernetes Secrets (#93, #94).
- **Opt-in services** with `--services`: Firestore, Datastore, Bigtable and Spanner on Google's
  emulators (#75, #371); BigQuery on a community emulator (#323); Cloud SQL for PostgreSQL and
  MySQL data planes (#122, #297, #311); Memorystore on Valkey (#296); Cloud Scheduler (#302);
  Cloud Logging write and read (#304); and Cloud KMS with symmetric keys, Encrypt and Decrypt,
  the version lifecycle and REST transcoding (#385–#424). Cloud KMS is not a security boundary.
- Resource Manager v1 and v3 projects (#298, #301), local credentials and a GCE metadata server
  (#92), and IAM Credentials token generation from the local signer (#303).
- IAM policies stored, never enforced, on Secret Manager secrets and Cloud Tasks queues
  (ADR-0006, #308, #365, #366).
- **Vertex AI on local runtimes**: a `generateContent` subset verified through the official
  `genai` SDK (#105, #106) and custom prediction containers (#90).
- **A local web console** laid out after the Google Cloud console, with detail pages, actions
  where the backend supports them, SQL and query editors, Kubernetes views, metrics, a Logs
  Explorer and a request log (#97 onward).
- An admin API for reset, seed and event inspection (#70, #317, #319, #320, #334), state save
  and load (#338, #340), lifecycle hooks (#333), fault injection (#306), request logging
  (#314), request metrics (#341) and OpenTelemetry traces (#313, #379).
- The setup-cloudburrow GitHub Action (#330), `env --format terraform` and
  `--format docker-compose` (#332).
- SDK compatibility suites for Go and Python (#56, #335), and per-service API coverage generated
  from the proto surface (#337).
- Release packaging: archives for macOS and Linux on arm64 and amd64 with build attestations,
  one `checksums.txt`, a Homebrew formula and an installer that refuses an archive it cannot
  verify (#71, #324).

[Unreleased]: https://github.com/cloudburrow/cloudburrow/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0
