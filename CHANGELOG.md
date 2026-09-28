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

- **Pub/Sub subscriptions expire** (#873): a front before Google's emulator, in the Pub/Sub pod,
  deletes a subscription idle for its `expirationPolicy.ttl` (31 days when none is given, as in
  Google), counts every call naming it as activity and an open streaming pull as keeping it
  active, and refuses a ttl under a day or under the message retention. Exactly-once delivery,
  which the emulator implements, is now verified with the official client. The console's
  **Create subscription** offers both. A Pub/Sub instance now needs a CLI with the embedded Linux
  binaries (`make build` or a release), because the front runs from the storage image. The front
  serves the emulator's REST API on the same port too, with the same rules, so gcloud and
  Terraform, which use REST, reach Pub/Sub as before.
- **Pub/Sub in `cloudburrow state save` and `load`** (#880): schemas with every revision, topics,
  subscriptions with every setting and their expiration clocks, and snapshots, in every project a
  call has named; messages are not kept. Push subscriptions now expire too: the front relays the
  emulator's pushes and counts a successful one as activity. Exactly-once delivery with a push
  endpoint is refused, as Google refuses it, on the API and in a seed file, which now accepts
  `enableExactlyOnceDelivery` on a pull subscription; **Edit subscription** turns it on and off.
  All of it holds for REST clients (gcloud, Terraform) too (#908): a push endpoint sent over REST
  is relayed and read back as sent, exactly-once with push or export is refused on REST, and a
  project named only in REST paths is saved.
- **Pub/Sub expiration can be changed** (#891): the emulator refuses an `UpdateSubscription` of
  `expiration_policy`, so CloudBurrow's front applies it, checks it by Google's rules, returns it
  on every read and enforces it, over gRPC and REST alike (gcloud's `subscriptions update
  --expiration-period`, #908); `state save` keeps it. **Edit subscription** now edits the
  expiration period.
- **Pub/Sub schemas in a seed file** (#890): `pubsub.schemas` declares Avro schemas and a topic's
  `schemaSettings` binds it to one, which the emulator enforces on publish.
- **A terminal in the console's top bar, like Cloud Shell** (#781): **Activate terminal** opens
  a drawer with a shell in a pod in the instance's cluster (never on this machine), from the
  pinned Cloud SDK image with kubectl, with the instance's pod environment and gcloud
  configuration and no credential. It follows the toolbar's project, keeps its session across
  a closed drawer or a reload, and says why when there is no shell. The WebSocket is refused to
  any other origin, host or remote peer. What differs from Cloud Shell is in
  [docs/compatibility.md](docs/compatibility.md#console).
- **A release is latest only once its smoke test has passed** (#680): the release workflow
  publishes it as a prerelease, smoke-installs it on Linux and macOS, and only then marks it
  latest and pushes the formula to the Homebrew tap, so the install script, the action's
  `version: latest` and `brew upgrade` never get a release that failed it.
  [docs/install.md](docs/install.md#a-release-failed-its-smoke-test) says how to withdraw one.
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
- **`cloudburrow prefetch` and `up --offline`** (#604): prefetch stores in the state directory
  the kind node image, the storage image this CLI builds, every enabled backend's image by its
  pinned digest, Knative's YAMLs checked against their pinned sha256, and the images those YAMLs
  name. `up` prefers the cached copies; `up --offline` uses nothing else and, when something is
  missing, refuses before creating a cluster and names it. `doctor` reports whether each
  registry and GitHub is reachable, and whether the cache is complete. See "Offline and
  air-gapped use" in docs/install.md for the procedure, every artifact's measured size, and what
  is and is not tested. The Bigtable emulator no longer falls back to installing itself when
  its container starts.
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
- Cloud KMS automatic rotation (#816): `rotation_period` and `next_rotation_time` on
  `CreateCryptoKey` and `UpdateCryptoKey`, validated as resources.proto says (a period of 24
  hours to 876,000 hours, and a next rotation time with it); at the next rotation time the key
  gets a new primary version and the time moves on by the period. The console's **Edit key**
  offers both.
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
- A fault rule on Cloud Tasks' `ListQueues`, Secret Manager's `ListSecrets` or Cloud
  Scheduler's `ListJobs` now also fails that service's console list, with the message an SDK
  receives, and a console open on the screen draws from the rule's count (#594).
- The console is tested in headless Chrome in CI (`test/browser`, chromedp): shell landmarks,
  the theme switch, creating a bucket through the form, the error card under an injected fault,
  keyboard row activation, and no request off loopback (#594). docs/console-verification.md
  describes the run in place of the manual transcript.

### Fixed

- **A Pub/Sub reset deletes schemas** (#889): `/admin/reset` for Pub/Sub deleted a project's
  subscriptions, snapshots and topics and left its schemas, so a test that reset between cases
  met `ALREADY_EXISTS` creating the same schema again.
- **BigQuery refuses what BigQuery refuses** (#861): invalid dataset and table IDs, field names
  and duplicate columns are 400, a duplicate dataset is 409, not the emulator's 500 that the Go
  client retried until its deadline; `tabledata.insertAll` checks every row against the schema
  and honours `skipInvalidRows` and `ignoreUnknownValues`, with per-row `insertErrors`, so a bad
  value no longer leaves a table that cannot be read. The checks run in front of the emulator
  on the host tunnel; see [docs/compatibility.md](docs/compatibility.md#bigquery-is-a-community-emulator).
- **BigQuery's checks reach every pod, and cover ALTER TABLE, CREATE TABLE ... AS SELECT,
  script blocks and autodetected loads** (#901, #902): the validating front runs in the
  emulator's pod, from the storage image, so `bigquery.<namespace>.svc.cluster.local:9050` is
  checked with or without Cloud Run; enabling BigQuery now needs a CLI with the embedded storage
  server, as Cloud Storage does. ALTER TABLE and IF, LOOP, WHILE, REPEAT, FOR and CASE blocks,
  which the emulator reported done without doing, are 501; their names, a CREATE TABLE ... AS
  SELECT's columns and the columns a CSV load detects are held to BigQuery's rules.
- **BigQuery views, CREATE OR REPLACE, scripts and loads** (#916, #917, #918, #919): a view's
  ID and the columns its query gives are held to BigQuery's rules; CREATE OR REPLACE TABLE and
  VIEW replace an existing one, which the emulator refused; a CREATE TABLE ... AS SELECT that
  names a script variable is checked after its script; a failing script with an EXCEPTION
  handler, RAISE, materialized views and a view's column list are 501. Loads with no
  `sourceFormat` are CSV, as in BigQuery, instead of the emulator's 400 or 500; resumable
  uploads (the Go client's above 16 MiB) work; `gs://` loads read the instance's Cloud Storage
  instead of dialling Google's; a CSV whose first row BigQuery would not take as a header is
  501; and jobs.list reports the jobs the front failed.
- **BigQuery CSV loads, IF NOT EXISTS, failed jobs, LIKE/COPY/CLONE** (#931, #932, #934,
  #937): a CSV load with a schema, or into an existing table, loads every row, where the
  emulator silently dropped the first as a header whatever `skipLeadingRows` said (a `gs://`
  one is 501 unless `skipLeadingRows` is 1); CREATE TABLE and VIEW ... IF NOT EXISTS of one
  that exists does nothing, as in BigQuery, instead of failing; a job the emulator failed reads
  back failed through jobs.get and jobs.list instead of succeeded; CREATE TABLE ... LIKE, COPY
  and CLONE and snapshot tables are 501 instead of the emulator's 400.
- **BigQuery script variables, failed scripts, TEMP tables, job text and extracts** (#933,
  #935, #936, #938, #939): a script's variables end with the script, where the emulator kept
  them and put their values into every later query; a script that fails after a statement that
  changes data is 501, as the emulator rolls it all back where BigQuery keeps the earlier
  statements; CREATE OR REPLACE and DROP of a TEMP table the script made are 501 instead of the
  emulator's 400; a CREATE TEMP TABLE ... AS SELECT that names a variable or another TEMP table
  has its columns checked; a job the front rewrote shows the client's query in jobs.get; and a
  CSV extract to Cloud Storage writes BigQuery's file to the instance's own Cloud Storage (a
  wildcard URI's first file named as BigQuery names it, a missing bucket 404 instead of created,
  a view and a nested schema refused), with JSON, Avro, Parquet, compression, other delimiters
  and several URIs 501.
- A CLI built without the embedded Cloud Storage server (a plain `go build` or `go install`)
  is refused before `up` creates a cluster, naming the fix, rather than after kind has spent
  minutes creating one (#686). `cloudburrow doctor` and `diagnose` report it in an
  `embedded storage` row: which Linux builds are embedded and whether one is the node's. An
  empty or placeholder file counts as missing.
- Port forwarding replaces a dead tunnel on evidence rather than on one client's failure
  (#526), retries a launch that does not carry (#572), and notices a pod that restarted its
  containers (#566).
- **The setup-cloudburrow action works with the CLI it installs** (#679): it reads `cloudburrow
  version --short` and passes `--trust` and `--port-base` only to a CLI that has them, so
  `version: v0.1.0` (and `latest` while that is v0.1.0) no longer fails with "flag provided but
  not defined". The `port-base` input with v0.1.0 fails before `up`, naming the version it needs.
  Installing any release through the action also failed before this ("The \"path\" argument must
  be of type string"): it looked for its own files in `GITHUB_ACTION_PATH`, which the runner sets
  only for composite actions. It now finds them from its own location, which also makes
  `version: source` build the action's checkout rather than the job's workspace.
  The action self-test also runs against the newest published release, and the release
  workflow runs it against each tag it publishes. [docs/ci.md](docs/ci.md#which-cli-versions-an-action-ref-supports)
  has the table.
- **Both Linux release archives are linked the same way** (#714): every release CLI is built
  with `CGO_ENABLED=0`, and the release fails unless `go version -m` on each binary says so.
  The linux/amd64 binary was linked against the build runner's glibc; it is now static, like
  linux/arm64. Cloud Tasks and Cloud Scheduler HTTP targets under `.localhost` are dialled on
  `127.0.0.1` without a DNS lookup, so they no longer depend on the host's resolver.
  [docs/install.md](docs/install.md#how-the-cli-is-linked) records the linkage and the measured
  resolver behaviour.
- **Container engines are stated, and an engine whose kind gateway is not on this machine fails
  early** (#712): [docs/install.md](docs/install.md#container-engines) marks each engine
  supported, unverified or unsupported, with the evidence for each supported row. `doctor` names
  the engine from `docker info` and warns on rootless and non-Desktop VM engines, and no longer
  measures the host's `/` as the daemon root when a Linux client talks to Docker Desktop's VM.
  With Cloud Run, `up` checks that the kind gateway can be bound before relaying on it, and
  otherwise fails naming the engine and the workaround instead of with a bare listen error.

### Security

- Every `/admin` route now requires a per-instance token (#553).
- Every HTTP listener refuses a request whose `Host` is not an IP address, `localhost` (or a
  name under `.localhost`), `host.docker.internal`, `cloudburrow-host.<namespace>.svc.cluster.local`
  or, on the builtin storage server, its Service and virtual-hosted bucket names, answering 421
  with the rejected host named (#676). This is the DNS-rebinding defence: a page on a domain
  that resolves to 127.0.0.1 could otherwise drive the console and the service APIs as a
  same-origin peer. Cleartext HTTP/2, and so gRPC, is not checked, because a browser never sends it.
- The builtin Cloud Storage server refuses a browser request from an origin that is neither
  loopback (`localhost`, `*.localhost`, `127.0.0.1`, `[::1]`, any port) nor named with the new
  `up --cors-allow-origin` (`CLOUDBURROW_CORS_ALLOW_ORIGIN`, config `storage.corsAllowOrigins`),
  answering 403 with no CORS headers, preflights included (#677). Its JSON API had allowed every
  origin, as Google's does, so any web page could read and change local buckets. A request with no
  `Origin`, as the SDKs and `curl` send, is unaffected. `Access-Control-Allow-Credentials` is no
  longer sent: no recorded observation of Google shows it.

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
