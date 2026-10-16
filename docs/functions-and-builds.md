# Functions and source builds

Two optional capabilities, both reusing Google-published components, and the Cloud Functions
v2 API built on them (#1377, [below](#the-cloud-functions-v2-api-1377)). The other
Google API that builds through them is Cloud Run's `Builds.SubmitBuild` (#1268), which builds
a source archive from the instance's Cloud Storage with Docker or this pack path; see the
Cloud Run section of [compatibility.md](compatibility.md).

**`gcloud run deploy --source` (#1347).** gcloud 586 creates the `cloud-run-source-deploy`
Artifact Registry repository, uploads the source (a zip) to `run-sources-{project}-{region}`,
calls SubmitBuild and polls Cloud Build's operation for the build. Each build SubmitBuild runs
is a Cloud Build `Build` (`GetBuild`, `ListBuilds`, its operation at
`projects/*/locations/*/operations/{build id}`), and the repository is Artifact Registry
metadata (`GetRepository`, `CreateRepository`, `ListRepositories`), both served on the Run port
and reached through `gcloud-setup`'s `cloudbuild` and `artifactregistry` overrides. The build
itself stays local: Docker for a Dockerfile, pack for a source without one. Since #1383 the
image is **pushed** to the repository, a Docker registry ([below](#artifact-registrys-docker-registry-1383)),
and the digest the build reports is its manifest's; it is also loaded into the cluster as
`dev.local/<image URI>`, the fast path, so a service naming the image under the repository's
URL, by tag or by that digest, runs it without a pull. The console
shows the repositories (Artifact Registry) and the builds (Cloud Run, Build history), each
build's page with its steps' status and timing and its log.

**Cloud Build `builds.create` and `gcloud builds submit` (#1347, #1382).** `CreateBuild` (global
and regional), `CancelBuild` and `RetryBuild` are served on the Run port. A build's source is
`source.storageSource`, a tar.gz or zip in the instance's Cloud Storage. These steps run through
the same local builder as SubmitBuild, not in the step's own container:

- `gcr.io/cloud-builders/docker` `build` (`-t`, `-f`, `--build-arg`, a context directory;
  `--network`, `--cache-from`, `--no-cache` and `--pull` change nothing here) builds with the
  host's Docker for the nodes' architecture and loads the image as `dev.local/<tag>`;
- `gcr.io/cloud-builders/gcb-internal` with `docker build …` as its args, the step
  `gcloud builds submit --tag` sends, is the same docker build;
- `docker push` of a tag an earlier step built pushes it to its Artifact Registry repository
  when this instance holds that repository (#1383), and is otherwise a success that only
  leaves the image in the cluster. Each of `images` is pushed the same way and gets its
  manifest's digest (a digest derived from the build's ID when nothing was pushed), and a
  service naming it by tag or digest runs it;
- `gcr.io/k8s-skaffold/pack` `build IMAGE` (`--builder gcr.io/buildpacks/builder…`, `--env`,
  `--path`) builds with pack, gated as the Functions tests are.

**Every other step runs in its own container (#1382)**, as Google's build worker runs it: a
container of the step's `name` on the host's Docker (pulled when missing, logged as
`Pulling image: …`), with

- the Docker socket mounted at `/var/run/docker.sock`, so a step can run `docker` itself;
- the user-defined network `cloudbuild`, made on the first build that needs it and removed after
  the last, so containers a step starts with `--network cloudbuild` reach each other;
- the build's `/workspace`, a Docker volume holding the unpacked source, as the working directory
  (`dir` under it); what a step writes there, every later step sees, and a docker build or pack
  step after it builds that `/workspace`, not the original archive;
- `/builder/home` (`HOME`), shared by every step, and `/builder/outputs` (`BUILDER_OUTPUT`);
- the step's `entrypoint`, `args`, `env` and `volumes` (named volumes, shared by the steps that
  name them; a path must be absolute and not `/workspace`, `/builder/home` or the socket), and
  `automapSubstitutions` (each substitution as an environment variable).

A non-zero exit fails the build with `build step N "name" failed: step exited with non-zero
status: C` and the step's `exitCode`. Steps run as `waitFor` orders them: a step without it waits
for every earlier step, `-` starts at once, and ids name the steps to wait for, so independent
steps run concurrently. The first failure stops the rest; a step running then is `CANCELLED`,
one never started stays `QUEUED`. A step's `timeout`, the build's, and `CancelBuild` kill the
step's container. The volumes and containers are removed when the build ends. Steps run on the
host's own architecture: Google's workers are linux/amd64. The step's container reaches the
host's network as any container does, so a step that calls a Google API, such as a
`gcr.io/cloud-builders/gcloud` step, calls Google, not this instance.

**Security: off by default, loopback only (ADR-0004, decided 2026-10-16).** A step gets the
host's Docker socket, as on Google, where the socket belongs to a throwaway VM. Here it belongs
to the developer's machine: a step can start any container, including a privileged one, with any
of the host's files mounted. So these steps run only when `up` is given
`--cloudbuild-host-steps` (`CLOUDBURROW_CLOUDBUILD_HOST_STEPS=true`, or `"cloudbuildHostSteps":
true` in a config file you name; a discovered `./cloudburrow.json` cannot set it). Without it, a
build with any step other than docker build, docker push or pack is refused before anything
runs, `FAILED_PRECONDITION`:

```
build step 0 (alpine) is not a docker build, docker push or pack step, and this instance runs
such steps only when started with --cloudbuild-host-steps: each one runs as a container on the
host's Docker with the host's Docker socket mounted, which gives it control of the host (ADR-0004)
```

Docker build, push and pack builds, `gcloud builds submit --tag`, source deploys and functions
are unaffected. With the flag on, such a build still runs only when it was submitted from this
machine's loopback: from a loopback address, to `127.0.0.1`, `[::1]` or `localhost`, as the SDK,
gcloud (through `gcloud-setup`), Terraform and the console's Retry do. A pod's `CreateBuild` or
`RetryBuild` through `cloudburrow-host` (or `host.docker.internal`, the kind gateway or a cluster
IP), and another machine's under `--allow-remote`, is refused `FAILED_PRECONDITION` with the
reason. The console's build history says whether the steps are on, and why. Do not run builds
from sources you would not run on the host.

Built-in (`$PROJECT_ID`, `$PROJECT_NUMBER`, `$BUILD_ID`, `$LOCATION`) and user (`$_NAME`)
substitutions apply to a step's `name`, `args`, `env`, `dir` and `entrypoint`, and to `images`.
So do a step's `id`, `waitFor` and `timeout`, and the build's `timeout` (default ten minutes).
Status goes `QUEUED`, `WORKING`, then `SUCCESS`, `FAILURE`, `TIMEOUT` or `CANCELLED`, with each
step's status and timing; the operation answers with the Build, or fails (`UNKNOWN`, `CANCELLED`,
`DEADLINE_EXCEEDED`). The log is written as Google writes it, `log-{build id}.txt` in
`logsBucket`; `options.logging` `NONE` or `CLOUD_LOGGING_ONLY` writes none.

**The default logs bucket (#1430).** A build without `logsBucket` reports Google's default,
`gs://{project number}.cloudbuild-logs.googleusercontent.com`, and its log is written there, as
on Google. Google owns that bucket, in a project of its own, and the instance's Cloud Storage
treats it the same way: Cloud Build makes it on the first log, through a path of the storage
server's own (`/_cloudburrow/cloudbuild-logs`) that skips the bucket-name check, since no user
may create a name containing `google` (or a domain name they cannot verify). Through the APIs
(JSON, XML, gRPC) it is read-only: `buckets.get`, `objects.list`, `objects.get` and downloads
are served, so the official client, `gcloud storage cat` and `gsutil cat` read the log, and any
write, delete, patch or copy into it is 403 Forbidden. It is not in the project's `buckets.list`,
nor on the console's Cloud Storage page. A log that cannot be written (a `logsBucket` that does
not exist, or Cloud Storage not running) is not dropped: Google documents no build field for it,
so it goes to the instance log (`Cloud Build could not write the build log`) once per build, and
the console's build page shows it as **Log write error**.

**`gcloud builds submit` without `--async`**, and `gcloud builds log`, read the log from
`https://storage.{core/universe_domain}/BUCKET/OBJECT` themselves, which no
`api_endpoint_overrides` property changes (gcloud's `GCSLogTailer`). `--suppress-logs` does not
help: it only stops printing, and the log is still polled from Google. So use `--async`, wait
with `gcloud builds describe`, and read the log with `gcloud storage cat
gs://BUCKET/log-ID.txt`, which honours the `storage` override `gcloud-setup` writes
(`TestGcloudBuildsSubmitDefaultLogsBucket`), or the console's build page.

**`ListBuilds` (#1382)** takes the `filter` "Filtering build results" documents: `status`,
`build_id`, `trigger_id`, `tags`, `images`, `results.images.name`, `results.images.digest`,
`source.storage_source.bucket` and `.object`, `source.repo_source.*`,
`source_provenance.resolved_repo_source.commit_sha`, `options.requested_verify_option`,
`create_time`, `start_time` and `finish_time`. `=` and `!=` work on every field, and `>`, `>=`,
`<` and `<=` on the times (RFC 3339). Terms combine with `AND`, `OR`, `NOT` (or a leading `-`)
and parentheses; as in Google's list filters, `OR` binds tighter than `AND`. A repeated field
matches `=` when any value does, and `field=(a OR b)` is either. An unknown field or a malformed
filter is `INVALID_ARGUMENT`. `page_size` and `page_token` page the newest-first list; a token
is bound to the filter and location it was issued for. The global form (`projects/{p}/builds`)
lists every location and the regional form one. `gcloud builds list --filter` works through it.

**`RetryBuild` (#1382)** creates a new build, with a new ID and a new operation, from the request
the build was created with: its source, steps, substitutions, tags and options. A storage source
without a generation is read again, so the retry may see a newer object, as Google documents. A
build that SubmitBuild ran, for `gcloud run deploy --source` or a function, is refused
`FAILED_PRECONDITION`, naming why: its request was Cloud Run's, so there is no Cloud Build request
to rerun. Deploy again to rebuild it.

**Secrets, scripts and failures a build allows (#1407)**, on the arbitrary steps
`--cloudbuild-host-steps` runs:
- `availableSecrets.secretManager` (`versionName`, substitutions applied, and `env`) is read
  from this instance's Secret Manager when the build runs; `availableSecrets.inline` and the
  legacy `secrets` (`kmsKeyName` with `envMap` or `secretEnv`) are decrypted with this instance's
  Cloud KMS. A step's `secretEnv`, and `options.secretEnv` for every arbitrary step, names which
  of them the step gets as variables; refer to one in `args` as `$$NAME`, with a shell
  `entrypoint`, as Google documents. A name nothing defines, a duplicate, or a malformed
  `versionName` or `kmsKeyName` is `INVALID_ARGUMENT`; a secret the build names on an instance
  without Secret Manager (or Cloud KMS) is `FAILED_PRECONDITION`, naming the service to enable;
  one that cannot be read fails the build before its steps run, naming the version.
- Cloud Build does not mask secrets in build logs: Google's documentation says nothing of it, so
  a step that prints a secret shows it in the log, on Google and here (the logs bucket's
  `log-{id}.txt` and the console's log view). Matching Google here is the user's decision
  (2026-10-16); keep secrets out of a step's output. A step's secrets are
  passed to `docker` through its environment as `-e NAME`, so no value is on a command line; they
  are in the step container's environment while it runs, as on Google's worker.
- A step's `script` is written to `/builder/script`, executable, and run by its `#!` line's
  interpreter, else by `/bin/sh`, as Google documents. Substitutions are not applied inside a
  script; `automapSubstitutions` (the step's, `true` or `false`, wins over `options`') gives them
  as variables. `script` with `args` or `entrypoint` is `INVALID_ARGUMENT`.
- `allowFailure`, and `allowExitCodes` (codes 0 to 255, which take precedence), leave a failed
  step `FAILURE` with its `exitCode` while the build goes on, and the build is `SUCCESS` if
  nothing else fails; steps waiting for it start. A step that times out still fails the build.
- `options.env` is given to every arbitrary step, a step's own `env` overriding it.

**Refused, with the reason** (`UNIMPLEMENTED`): `env`, `volumes` and `secretEnv` on a docker
build, docker push or pack step (the local builder runs those, not the step's container: run
`docker` from a step with `entrypoint: bash` to give it a secret); the build's `artifacts`,
`serviceAccount`, worker pools, machine types, `options.dynamicSubstitutions` and other
`options`; and sources other than `storageSource`. Triggers,
worker pools and `ApproveBuild` are not served. The console's Build history takes the same
filter in its filter box, and offers Retry on a finished build's page.

**Build triggers and private worker pools (#1422).** A trigger is a stored build. A manual
trigger (`RunBuildTrigger`, `gcloud builds triggers run`) and a webhook trigger
(`webhookConfig.secret`, `POST …/triggers/{trigger}:webhook?secret=…`) run their inline `build`
as `CreateBuild` does, with the trigger's substitutions, the run's `source.substitutions` over
them, `$TRIGGER_NAME` and `buildTriggerId`. A webhook's `secret` must equal the Secret Manager
version the trigger names (403 otherwise), and `$(body.field)` bindings take the payload's
values. Make one with `gcloud builds triggers create manual --inline-config=build.yaml` or
`create webhook --secret=projects/P/secrets/S/versions/1 --inline-config=build.yaml`, Terraform's
`google_cloudbuild_trigger` with a `build` block, or the console's Cloud Build Triggers page.

What needs a live service is refused with the reason: GitHub, GitLab, Bitbucket and Cloud
Source Repositories events, a build read from a repository (`filename`, `gitFileSource`,
`sourceToBuild`), since Google clones those from a hosted git service and not from a path on
your machine, and `pubsubConfig`, whose subscription Google owns. To build on a Pub/Sub message,
point a push subscription at the webhook URL.

A worker pool (`gcloud builds worker-pools create`, `google_cloudbuild_worker_pool`, the console's
Worker pools page) is stored with its long-running operations. A build that names it in
`options.pool.name` runs on the host's Docker like any other, with the pool recorded on the build.
Its machine type, disk and network are stored and not acted on.

## Artifact Registry's Docker registry (#1383)

Each `DOCKER` repository is a Docker registry: the Docker Registry HTTP API v2 (the OCI
distribution spec) at `/v2/` on the Run port, over a content-addressed store in the instance's
state directory (`<state-dir>/<name>/artifactregistry/`), which survives a restart with the
repositories. On Google the registry is `{region}-docker.pkg.dev/{project}/{repository}/{image}`;
here a docker client reaches it as

    {region}-docker.pkg.localhost:{run port}/{project}/{repository}/{image}

`*.localhost` resolves to loopback without any DNS or `/etc/hosts` entry (RFC 6761), and docker
and crane treat a loopback registry as insecure (plain HTTP) without configuration, as they do
`localhost:5000`. The region in the name selects the repository's location; `127.0.0.1:{run
port}` works too and finds the project's one repository of that ID. `cloudburrow gcloud-setup`
prints the address. For example, with the `demo` instance's repository `images` in
`us-central1`:

    docker tag app us-central1-docker.pkg.localhost:9004/demo/images/app:v1
    docker push us-central1-docker.pkg.localhost:9004/demo/images/app:v1
    crane ls us-central1-docker.pkg.localhost:9004/demo/images/app

No credential is asked for, so `gcloud auth configure-docker` is not needed. CloudBurrow never
writes docker's configuration (`~/.docker/config.json`): if you have run `gcloud auth
configure-docker us-central1-docker.pkg.dev`, that names the real host and is not consulted
for the `.localhost` one. To push with the `-docker.pkg.dev` name itself you would have to map
it to this machine yourself (an `/etc/hosts` entry, and docker's `insecure-registries`); nothing
here does that for you, because it would also send your real pushes here.

What is pushed is what the Artifact Registry API reads: packages, versions, tags, Docker images
and files (`gcloud artifacts docker images list`, `gcloud artifacts docker tags add`), and the
console's repository page lists images, versions, tags and files with Delete, Add tag and
Remove tag. Cloud Build's `images` and `docker push` steps, and SubmitBuild, push to it. A Cloud
Run service naming an image the registry holds, and that was not built here, pulls it: the
revision names it by digest, and every kind node's containerd has a
`/etc/containerd/certs.d/{region}-docker.pkg.dev/hosts.toml` (written at `up`, for every
region) whose server and mirror are the Run port at the address pods reach this machine at, so
a pull never falls back to Google. See the Docker Registry rows of
[compatibility.md](compatibility.md) for what is served and refused.

## Functions Framework (#32)

An ordinary [Functions Framework](https://github.com/GoogleCloudPlatform/functions-framework-go)
function runs unchanged. The fixtures in `testdata/function` (Go), `testdata/function-nodejs`
(Node.js), `testdata/function-python` (Python), `testdata/function-java` (Java),
`testdata/function-ruby` (Ruby), `testdata/function-php` (PHP) and `testdata/function-dotnet`
(.NET) contain no CloudBurrow-specific code — that is the point.

**Languages.** Go, Node.js, Python, Java, Ruby, PHP and .NET are Verified. Node.js and Python
(#1303) and Java, Ruby, PHP and .NET (#1319) have fixtures and gated tests,
`TestFunctionsNodejsBuiltWithBuildpacks`, `TestFunctionsPythonBuiltWithBuildpacks`,
`TestFunctionsJavaBuiltWithBuildpacks`, `TestFunctionsRubyBuiltWithBuildpacks`,
`TestFunctionsPHPBuiltWithBuildpacks` and `TestFunctionsDotnetBuiltWithBuildpacks`, that build
them with the pinned builder and deploy them through the Cloud Run v2 client. All six passed in
one dated run: 2026-10-15, [run 38805202491](https://github.com/cloudburrow/cloudburrow/actions/runs/38805202491) of `functions.yml` on `fix/1319-php-function`, `ubuntu-24.04` amd64 runner, pack 0.40.9. They need amd64 nodes, so on every change what is checked is
that `buildpacks.Detect` names each fixture's language from its manifest
(`TestDetectTheFunctionFixtures`).

**PHP uses Google's PHP functions builder.** `gcr.io/buildpacks/builder` has no PHP Functions
Framework buildpack: given `GOOGLE_FUNCTION_TARGET` it still builds a PHP web application, nginx
and php-fpm running `index.php` directly, so the framework's router and autoloader never load
and a function fails with `Class "Google\CloudFunctions\FunctionsFramework" not found`. Google
builds PHP functions with its per-language builder,
`us-docker.pkg.dev/serverless-runtimes/google-22-full/builder/php` (pinned by digest in
`dependencies.json`), whose functions-framework and cloudfunctions buildpacks serve
`vendor/google/cloud-functions-framework/router.php`. `internal/buildpacks` picks it for a build
with a function target whose runtime is PHP; every other build uses the general builder (#1319).

| Signature | Status | Evidence |
|---|---|---|
| `http` | **Verified** | `TestFunctionsFrameworkBuiltWithBuildpacks`: `POST /` returns the handler's JSON; a body-less request still answers. Verified by a dated CI run (2026-09-27, run 36357529139, amd64); gated |
| `cloudevent` | **Verified** | `TestFunctionsFrameworkBuiltWithBuildpacks`: a Google-schema CloudEvent in binary mode reaches the handler, which logs its type, subject and data. Verified by the same run; gated |
| Errors | **Verified** | `TestFunctionsFrameworkBuiltWithBuildpacks`: a request without `ce-*` headers to a CloudEvent function returns **HTTP 400**. Verified by the same run; gated |
| Java, Ruby, PHP and .NET, `http` and `cloudevent` | **Verified** | The four #1319 tests assert what the Node.js test does: the greeting, a body-less 200, and the handler logging the event's type, subject and data. No HTTP 400 is asserted for a request with no `ce-*` headers: nothing has measured these frameworks' answer. Verified by a dated CI run (2026-10-15, run 38805202491, amd64); gated |
| Node.js and Python, `http` and `cloudevent` | **Verified** | `TestFunctionsNodejsBuiltWithBuildpacks` and `TestFunctionsPythonBuiltWithBuildpacks` assert what the Go test does for `http` (the greeting, a body-less 200) and `cloudevent` (the handler logs the event's type, subject and data). Only the Python test asserts the HTTP 400 for a request with no `ce-*` headers: the Node.js framework (5.0.5) hands such a body to the function as a structured-mode event instead. Verified by the same run (2026-10-15, run 38805202491, amd64); gated |

The tests are in `test/compat` and run only with `CLOUDBURROW_TEST_FUNCTIONS=1`, against an
instance whose cluster nodes are amd64, with `pack` and Docker installed. They are too slow for
every merge, so no CI shard runs them; see
[compatibility.md](compatibility.md#what-ci-does-not-run-and-why). They run on demand in
[`functions.yml`](../.github/workflows/functions.yml) (`workflow_dispatch` only). The Go rows are
Verified by one dated run: 2026-09-27, [run 36357529139](https://github.com/cloudburrow/cloudburrow/actions/runs/36357529139) of `functions.yml` on main, `ubuntu-latest` amd64 runner, pack 0.40.9; the test passed in 136s, building `Hello` (`http`) in 58s and
`Event` (`cloudevent`) in 39s. The Node.js, Python, Java, Ruby, PHP and .NET rows are Verified
by 2026-10-15, [run 38805202491](https://github.com/cloudburrow/cloudburrow/actions/runs/38805202491) of `functions.yml` on `fix/1319-php-function`, `ubuntu-24.04` amd64 runner, pack 0.40.9, in which all ten Functions tests passed. Nothing re-runs them on merge.

Built images deploy through the established Knative path, like any other image.

### How the runtime is chosen

As on Google, the builder chooses: `GOOGLE_RUNTIME` in the build's environment (`--env` to
pack, or `buildpack_build.environment_variables` in `SubmitBuild`) forces a runtime, such as
`nodejs` or `python`; without it the builder detects one from the source. Before pack runs,
`buildpacks.Detect` reads the same signals:

- `GOOGLE_RUNTIME`, which wins and reaches pack unchanged (a version suffix, as in `nodejs22`, is
  kept apart in the report);
- a manifest at the root: `go.mod`, `package.json`, `requirements.txt`, `pyproject.toml`,
  `setup.py`, `pom.xml`, `build.gradle(.kts)`, `Gemfile`, `composer.json`;
- otherwise a source file anywhere in the tree (`.go`, `.js`, `.py`, `.jar`, `.rb`, `.php`,
  `.csproj`), skipping `node_modules`, `.git` and virtualenvs.

CloudBurrow never sets `GOOGLE_RUNTIME` itself, so a source that matches several runtimes is
built as the builder's own order decides. A source with no marker at all is refused before
any image is pulled, with `build failed: no runtime detected` naming the files looked for and
`GOOGLE_RUNTIME`. A failed build names the runtime it was built as, such as
`build failed: exit status 1 (runtime nodejs, detected from package.json)`, followed by the
tail of pack's log. `SubmitBuild`'s `buildpack_build.runtime` stays `UNIMPLEMENTED`.

**Event triggers** (#1414): a function's `eventTrigger` on Pub/Sub
(`google.cloud.pubsub.topic.v1.messagePublished`) or Cloud Storage
(`google.cloud.storage.object.v1.finalized`, `deleted`, `archived`, `metadataUpdated`, filtered
on `bucket`) is made as Eventarc makes it, once the service is deployed: an `eventarc-…` push
subscription on the topic (for Cloud Storage, on an `eventarc-…` topic the bucket's notification
publishes to), whose pushes this instance converts to binary-mode CloudEvents (`ce-*` headers,
`MessagePublishedData` or the object resource) and POSTs to the function through the ingress, with
an ID token for the trigger's service account. `eventTrigger.trigger` names the trigger. It needs
`--services run,storage,pubsub` and a published ingress port. No Eventarc API is served, and
other event sources are refused. See the `eventTrigger` row of `docs/compatibility.md`.

## The Cloud Functions v2 API (#1377)

`google.cloud.functions.v2.FunctionService` is served on the Run port, over gRPC and as JSON at
Google's v2 paths, and reached by `gcloud functions … --gen2` through `gcloud-setup`'s
`cloudfunctions` override, by Terraform's `google_cloudfunctions2_function` through
`cloudfunctions2_custom_endpoint`, and by the console's Cloud Functions page. It needs
`--services run,storage`: the source is kept in Cloud Storage.

A function is what Google's 2nd gen makes of it, a build and a Cloud Run service:

1. **Source.** `GenerateUploadUrl` returns a URL on the Run port that takes the zip with an
   unauthenticated `PUT`, as Google's signed URL does, and writes it to
   `gs://gcf-v2-uploads-{project number}-{region}/{uuid}.zip` in the instance's Cloud Storage
   (the bucket is made on the first upload; Google's is named
   `gcf-v2-uploads-{project number}.{region}.cloudfunctions.appspot.com`). Any other
   `build_config.source.storage_source` in the instance's Cloud Storage works too.
   `GenerateDownloadUrl` returns a URL on the Run port that reads the source back.
2. **Build.** `CreateFunction` stores the function `DEPLOYING` and returns its operation
   (`OperationMetadata` with the `BUILD` and `SERVICE` stages and `build_name`). The build is
   SubmitBuild's buildpacks build, so it is a Cloud Build build in the build history:
   `GOOGLE_RUNTIME` is `build_config.runtime` (one `ListRuntimes` names), the function target
   `entry_point`, plus `build_config.environment_variables`. The image is named
   `{region}-docker.pkg.dev/{project}/gcf-artifacts/{function}:version_…`, as Google names it,
   and loaded into the cluster, not pushed. Builds are gated as the Functions tests are: Google's
   builder makes linux/amd64 images only.
3. **Service.** The function's Cloud Run service has the function's ID, runs the image with
   `service_config`'s memory (Functions' `256M` is `256Mi`), CPU, timeout, environment,
   max/min instances and concurrency, and its service account is stored on it (the revision runs
   as the host). `service_config.service`, `uri`, `revision` and the function's `url` report it,
   and the function is `ACTIVE`. A failed build or rollout fails the operation and leaves the
   function `FAILED` with a state message, as on Google.

`UpdateFunction` with a mask redeploys what the mask touches: `build_config.*` rebuilds and rolls
out, `service_config.*` and `labels` roll out a new revision of the same image, and
`description` changes nothing that runs. With no mask the function is replaced and rebuilt.
`DeleteFunction` deletes the service; the image stays in the cluster, as Google leaves it in
`gcf-artifacts`. `getIamPolicy`, `setIamPolicy` and `testIamPermissions` on a function are stored
and never enforced (ADR-0006). `locations.list` names the regions; any well-formed region is
accepted in a name. Functions are kept in memory, and are gone when the instance restarts.

**Refused, with the reason** (`UNIMPLEMENTED`): an `eventTrigger` other than Pub/Sub's and Cloud
Storage's (above), or on an instance without Pub/Sub; GEN_1 functions and
uploads; `repo_source` and `git_uri`; worker pools, `docker_repository` and a build service
account; VPC connectors, secret environment variables and volumes, KMS keys and binary
authorization; `ListFunctions` `filter`, `order_by` and `page_token`; `GetFunction` `revision`.
An unknown runtime is `INVALID_ARGUMENT`. The tests are `TestFunctionsAPI` (ungated: everything but
a build that succeeds), `TestFunctionsEventTriggers` and `TestGcloudFunctionsEventTriggers`'
ungated half (event triggers, #1414), and `TestFunctionsAPIBuildsAndServes`,
`TestFunctionsEventTriggerBuilds`, `TestGcloudFunctions`'s and `TestGcloudFunctionsEventTriggers`'
deploys and `TestTerraformCloudFunctions2Function`'s apply, gated by `CLOUDBURROW_TEST_FUNCTIONS`
and amd64 nodes, which have **not yet run**.

## The Cloud Functions v1 API (#1428)

1st-gen functions are served through `cloudfunctions.googleapis.com/v1` on the same Run port, over
the same machinery: a v1 `CloudFunction` is stored as a v2 function whose `environment` is
`GEN_1`, built with the same buildpacks build and served as the same Cloud Run service. So the v2
API, `gcloud functions list` and the console see it as 1st gen, as Google shows it. What differs
is the v1 shape: `sourceArchiveUrl` (`gs://bucket/object`) or `sourceUploadUrl` (from v1's
`generateUploadUrl`), `httpsTrigger` (its `url` reported as Google names it,
`https://{region}-{project}.cloudfunctions.net/{name}`; the function is reached at its Cloud Run
service's URL), `eventTrigger` (`google.pubsub.topic.publish` on a topic, `google.storage.object.*`
on a bucket, both delivered as #1414's event triggers are), 1st gen's defaults (256 MB, 60 s, the
App Engine default service account) and v1's statuses (`DEPLOY_IN_PROGRESS`, `ACTIVE`, `OFFLINE`
after a failed deploy, `DELETE_IN_PROGRESS`), with `operations/{id}` operations. `call` POSTs its
`data` to an HTTP function through the ingress. `gcloud functions deploy --no-gen2` works through
`gcloud-setup`'s `cloudfunctions` override, which already names the Run port; it reads v2
`ListRuntimes` with `name=… AND environment=GEN_1`, which is served.

Refused with `UNIMPLEMENTED` and the reason: source repositories, VPC networks and connectors,
secrets, CMEK, build worker pools, `dockerRepository` and Container Registry, a build service
account, the legacy `object.change` and non-Pub/Sub, non-Storage event sources, call of an event
function (publish to its topic or write to its bucket instead), and past versions. A 1st-gen
function is updated through v1 only, and a 2nd-gen one through v2. `TestFunctionsV1API` and
the ungated halves of `TestGcloudFunctionsV1`, `TestConsoleFunctionsV1` and
`TestTerraformCloudFunctionsFunction` run on any instance with `--services run,storage,pubsub`;
`TestFunctionsV1BuildsAndServes` and those three tests' deploys from `testdata/function` are gated
like the other Functions tests and have **not yet run**.

The console's Create function offers the Environment as Google's does: 1st gen creates through
the v1 API with an HTTP trigger, a Pub/Sub topic or a Cloud Storage event, the function's page
shows the v1 `CloudFunction`, and Test function calls a 1st-gen HTTP function through `call`.
Terraform's `google_cloudfunctions_function` reaches the same API through
`cloud_functions_custom_endpoint`, which `cloudburrow terraform` sets.

## Google Buildpacks (#33)

`pack` plus Google's builder turns source into a runnable image with no Dockerfile.

**The builder is pinned by digest** and recorded in `dependencies.json`.

### The architecture constraint

**Google's builder is published for `linux/amd64` only.** On an arm64 host it runs under
emulation and produces an **amd64** image, which then needs an amd64-capable node to run.
CloudBurrow reports this rather than failing obscurely later.

That is not a refusal: a build on arm64 completed when tried by hand, though no automated
test covers the emulated path. It is slower, and the output is amd64.

### A registry is required

`pack`'s export to the Docker daemon fails on an arm64 host with this builder:

```
failed to fetch base layers: ... reference gcr.io/buildpacks/google-22/run:latest
was found but does not provide the specified platform (linux/amd64)
```

The build therefore always `--publish`es. **A local registry is not a cloud registry** — this
still satisfies "no cloud registry" and keeps everything on the machine:

```sh
docker network create cb-build
docker run -d --name cb-registry --network cb-build -p 127.0.0.1:5001:5000 registry:2

pack build cb-registry:5000/my-function:test --publish \
  --platform linux/amd64 --network cb-build --insecure-registry cb-registry:5000 \
  --path ./testdata/function \
  --builder gcr.io/buildpacks/builder:google-22 \
  --env GOOGLE_FUNCTION_TARGET=Hello \
  --env GOOGLE_FUNCTION_SIGNATURE_TYPE=http
```

The registry must share a Docker network with the build, because the buildpack lifecycle runs
**inside a container** — `127.0.0.1` there is the lifecycle container, not your machine.

### Behaviour and evidence

| Behaviour | Status |
|---|---|
| Build Node.js and Python source with no Dockerfile | **Verified** — `TestFunctionsNodejsBuiltWithBuildpacks` and `TestFunctionsPythonBuiltWithBuildpacks` build `testdata/function-nodejs` and `testdata/function-python`; Verified by a dated CI run (2026-10-15, run 38805202491, amd64); gated |
| Build Java, Ruby, PHP and .NET source with no Dockerfile | **Verified** — the four #1319 tests build `testdata/function-java`, `-ruby`, `-php` (with Google's PHP functions builder) and `-dotnet`; Verified by the same run (2026-10-15, run 38805202491, amd64); gated |
| Build Go source with no Dockerfile | **Verified** — `TestFunctionsFrameworkBuiltWithBuildpacks` builds `testdata/function` through `internal/buildpacks`; Verified by a dated CI run (2026-09-27, run 36357529139, amd64); gated |
| Resulting image runs and answers | **Verified** — the same test deploys it through the Cloud Run v2 client and expects `{"greeting":"hello CloudBurrow"}`; Verified by the same run; gated |
| Cache reuse on rebuild | **Verified** — `TestFunctionsRebuildReusesLayers` rebuilds unchanged source and asserts pack's log reports reused layers (`Reusing layer '…'`), and that the first build reported none; Verified by a dated run (2026-09-27, macOS/arm64 Docker Desktop, emulated amd64 builder, pack 0.40.9), the rebuild reusing 6 layers, and passed again in the dated CI run (2026-09-27, run 36357529139, amd64), again reusing 6 layers; gated, not run on merge |
| Failed build diagnostics | **Verified** — `TestFunctionsBuildBrokenModulePathFails` builds a `go.mod` with the module path `brokenmodule/function` and asserts the returned `build failed` error carries the buildpack's message naming that path; Verified by a dated run (2026-09-27, macOS/arm64 Docker Desktop, emulated amd64 builder, pack 0.40.9), and passed again in the dated CI run (2026-09-27, run 36357529139, amd64); gated, not run on merge |
| Initial download vs cached operation | First build pulls the builder and run images; rebuilds reuse layers (asserted by `TestFunctionsRebuildReusesLayers`; on the 2026-09-27 macOS run the first build took 1m48s and the rebuild 1m32s, both emulated; in CI run 36357529139, natively on amd64, 38s and 35s) |

The two build-only tests are in `test/compat` too and are gated by the same
`CLOUDBURROW_TEST_FUNCTIONS=1`. They need `pack` and Docker but no cluster, since they build
into a local registry and run nothing; like the test above, no CI shard runs them. Their
Verified status rests on two dated runs, both listed in
[What CI does not run](compatibility.md#what-ci-does-not-run-and-why): 2026-09-27 on macOS/arm64
Docker Desktop (emulated amd64 builder, pack 0.40.9), and 2026-09-27, [run 36357529139](https://github.com/cloudburrow/cloudburrow/actions/runs/36357529139) of `functions.yml` on main, `ubuntu-latest` amd64 runner, pack 0.40.9.

### Not covered

A re-run of the language tests on merge (the workflow is dispatch-only) · the emulated arm64 build path, which only a run by hand has
exercised (the gated test needs amd64 nodes) · fully offline builds, since the first build
must fetch the builder.
