# CloudBurrow in CI

CloudBurrow runs on a CI machine the same way it runs on a laptop: a kind cluster in Docker, with
`cloudburrow up` serving the APIs. Two recipes follow. The first uses the GitHub Action in this
repository, and the second is plain shell that works on any runner with Docker.

> `version` installs a published release, `latest` by default ([docs/install.md](install.md)).
> `version: source` builds CloudBurrow from the action's checkout instead, and needs Go.

## GitHub Actions

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1

      # kind and kubectl. Docker is already on GitHub's Ubuntu runners.
      - uses: helm/kind-action@06c1ae10762d3b9c1644e7fe69596ae519e015a2 # v1.15.0
        with:
          install_only: true
          version: v0.33.0
      - name: Install kubectl
        run: |
          curl -fsSLo kubectl "https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubectl"
          chmod +x kubectl && sudo mv kubectl /usr/local/bin/

      # Pin the action by commit SHA, as for any third-party action.
      - uses: cloudburrow/cloudburrow@<commit-sha>
        with:
          version: latest            # or a release tag (vX.Y.Z), or source (needs actions/setup-go)
          services: storage,pubsub   # default: CloudBurrow's defaults

      # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, GOOGLE_CLOUD_PROJECT and the
      # rest of `cloudburrow env` are now set for every later step.
      - run: go test ./...
```

What the action does:

1. **Installs CloudBurrow.** For a release, `scripts/install.sh` verifies the archive's SHA-256
   against `checksums.txt`, and its build attestation with the job's token. For `source`, it builds the
   embedded Linux storage servers and then the CLI from the action's checkout (`make
   storage-binaries`, then `go build`), so it needs Go and make (#623); with `bigquery` among the
   services it builds the embedded BigQuery emulator too (`make bigquery-binaries`, #1061), which
   takes minutes on a cold runner. The binary goes on `PATH`.
2. **Checks for Docker, kind and kubectl**, and names whichever is missing.
3. **Reads the installed CLI's version** (`cloudburrow version --short`), and passes only the
   flags that version has ([below](#which-cli-versions-an-action-ref-supports)).
4. **Runs `cloudburrow up --detach --trust`**, which returns once the instance is ready, then
   `cloudburrow wait`. `--trust` because the workflow is yours: a `cloudburrow.json` or
   `.cloudburrow/hooks` in the checkout is used without the prompt a laptop gives
   ([Trust](configuration.md#trust)). Hooks get a minimal environment; name what they need
   from the job's, such as `GITHUB_WORKSPACE`, with `hookEnv` in the config file.
5. **Appends `cloudburrow env --format plain` to `$GITHUB_ENV`.** Nothing needs configuring in
   test code for services that have an emulator variable. For those that don't (Cloud Tasks,
   Secret Manager, BigQuery), see [configuration.md](configuration.md#endpoints-and-sdk-configuration).
6. **Afterwards, always:** it prints `cloudburrow status --format json` and the last 200 log
   lines per container in collapsed groups, then runs `cloudburrow delete`. It checks the cluster
   is gone, and this runs whether the job passed or failed.

Inputs: `version` (`latest`), `services`, `mode` (`ephemeral`), `name` (`ci-<run id>-<job id>`),
`port-base` (empty: the default ports, from 9000), `timeout` (`10m`), `github-token`. Outputs:
`name`, `bin-dir`.

A distinct `name` gives each job its own cluster, but not its own host ports. On a GitHub-hosted
runner each job has a machine to itself, so that is enough. Jobs that can share a self-hosted runner
need a different `port-base` each, such as `9100` and `9200`
([configuration.md](configuration.md#running-two-instances)).

### Which CLI versions an action ref supports

The action's ref and `version` are chosen separately: an action pinned to a commit installs
`latest` unless `version` says otherwise, and `latest` may be older or newer than that commit.
From #679 on, the action reads the installed CLI's `cloudburrow version --short` and passes only
the flags that version has. It needs v0.1.0 or later, and refuses an older CLI before `up`,
naming v0.1.0.

| What the action passes | CLI that has it | With an older CLI |
| --- | --- | --- |
| `up --detach --detach-timeout`, `wait --timeout`, `env --format plain`, `status --format json`, `logs --tail`, `delete`; `--name`, `--mode`, `--services` | v0.1.0 | refused before `up`: the action needs v0.1.0 or later |
| `up --trust` (#598, #619) | v0.2.0 | left out. v0.1.0 has no trust gate and reads a discovered `cloudburrow.json` and `.cloudburrow/hooks` without it, which is what `--trust` asks for |
| `--port-base`, from the `port-base` input (#584) | v0.2.0 | the step fails before `up`, saying the input needs a CLI newer than v0.1.0. v0.1.0 moves ports only one by one, so there is nothing to translate it to |

`version: source` builds the CLI from the action's own checkout, which is the same commit, so it
gets every flag. A CLI whose version cannot be read, such as a local `dev` build, is taken to be
current and gets every flag, with a warning.

Action refs from before #679 always pass `--trust`, which v0.1.0 rejects ("flag provided but not
defined: -trust"). Use such a ref with `version: source` or v0.2.0 or later, or move to a
later ref. To keep a pinned action from picking up a newer CLI unannounced, set `version` to a
tag rather than relying on `latest`.

### Keeping logs from a failed run

The cleanup step runs after every step in the job, so anything it wrote would come too late for an
upload step to see. Collect diagnostics yourself in a step that runs only on failure, before the
job ends. `cloudburrow diagnose` writes a redacted bundle; its `manifest.json` lists what it
collected and what it could not:

```yaml
      - name: CloudBurrow diagnostics
        if: failure()
        run: |
          mkdir -p cloudburrow-diagnostics
          cloudburrow diagnose -o cloudburrow-diagnostics/bundle.tar.gz || true
      - uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1
        if: failure()
        with:
          name: cloudburrow-diagnostics
          path: cloudburrow-diagnostics
```

`cloudburrow logs` redacts credentials in what it prints ([configuration.md](configuration.md#logs)).

## Any runner, in shell

```sh
set -eu
# Install, verifying the checksum (and the attestation when gh is present).
curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh -s -- --prefix "$HOME/.local"
export PATH="$HOME/.local/bin:$PATH"

NAME="ci-${CI_JOB_ID:-$$}"
FLAGS="--name $NAME --mode ephemeral --services storage,pubsub"

# Delete the cluster however the script exits.
trap 'cloudburrow delete $FLAGS' EXIT

cloudburrow up --detach $FLAGS        # exits 0 only once ready; 1 failed, 2 timed out
cloudburrow wait --timeout 1m $FLAGS
eval "$(cloudburrow env $FLAGS)"

go test ./...
```

A `cloudburrow.json` or `.cloudburrow/hooks` in the working directory needs `up --trust` (or
`--config` and `--hooks-dir`, which name them) the first time; see
[Trust](configuration.md#trust).

`cloudburrow status --format json $FLAGS` gives a script the instance's state, and exits 0 ready,
3 not running or 4 not ready ([configuration.md](configuration.md#status-for-scripts)).

## What this repository runs

`.github/workflows/action-selftest.yml` runs the action with `version: source` and then a Go test
that creates a bucket and publishes to Pub/Sub, using nothing but the exported environment. A
second job fails a step on purpose. A third runs the action with `version: latest`, the newest
published release, and checks that it is ready and its environment exported, so the action is
tested against the CLI users get by default and not only the one built beside it. A last job
reads the logs of all three and requires the line the cleanup step prints once `kind get
clusters` no longer lists the cluster. That proves the cluster is deleted even when a test fails.
The first job also runs the action's unit tests (`node --test action/lib.test.js`), which cover
which flags each CLI version gets.

The release workflow calls the same self-test once a tag is published, with `version` set to that
tag, so each release is tested with the action at its own ref (#679).

### The upgrade test

`.github/workflows/upgrade.yml` ("Upgrade test", #1714) runs the end-to-end upgrade test,
`test/upgrade`, nightly at 05:37 UTC and by hand (`gh workflow run upgrade.yml`; `-f
older=v0.1.0` upgrades from another release the test lists in `olderReleases`). It downloads the
release the test names (`OlderRelease`, today `v0.2.0`) from this repository's GitHub release with
`gh release download` and the run's token, checks the archive against the release's
`checksums.txt`, builds this commit's CLI with `make build`, and runs
`TestUpgradeFromOlderRelease`: a persistent instance of sixteen services is brought up with the
older binary, seeded through the official clients, saved with `state save` and stopped, then
brought up from the same state directory with the new CLI and read back, and the archive is
loaded into it and read back. The run fails unless the test passed, so a skip does not count.

The first run (44094936759) took 11 minutes: 4.5 for `make build` with the emulator from `ci.yml`'s
cache, 5.5 for the test (about 8 more when the emulator must be built). That is a runner on every
landing, so it runs neither on pull requests nor on every push to main, and it is not a required
check. A failed run uploads `upgrade-diagnostics` (the test log,
the instance's `up.log` and its `cloudburrow diagnose` bundle), and a red scheduled run, or one
dispatched on main, opens a `workflow-red` issue through `report-red-runs.yml`.
`test/repo/upgrade_workflow_test.go` keeps the workflow's triggers, its download and its pass
check in shape.

### The Cloud SQL ephemeral nightly

`.github/workflows/nightly-sql-ephemeral.yml` ("Cloud SQL ephemeral nightly", #1752) runs the
compat job's `sql` and `sqlreplicas` shards, as `scripts/compat-shards.sh` defines them (services,
`-run`/`-skip` patterns and `go test -timeout`), each on an instance brought up with
`--mode ephemeral`, nightly at 06:47 UTC and by hand (`gh workflow run nightly-sql-ephemeral.yml`).
The compat job runs the shard on a persistent instance only, and a created Cloud SQL server keeps
its data differently in ephemeral mode: on a volume of its own that goes with the instance's record
(#1740). The run fails unless the whole suite passed and `TestCloudSQLInstanceWrites` (sql),
`TestCloudSQLDemoteSkipReplicationSetup`, `TestCloudSQLDemotePostgres`,
`TestCloudSQLExternalPostgresPrimary` and `TestCloudSQLExternalMySQLPhysical` (sqlreplicas), the
tests that failed only in this mode, passed rather than skipped. Run as one shard, without sql's
`-skip`, the suite passed its 60m deadline (#1813). The restart probes are not run: they measure persistent mode first, which the compat job
already does. It never runs on a pull request, a merge group or a push, it is not a required check,
and nothing in it reaches Google. A red scheduled run, or one dispatched on main, opens a
`workflow-red` issue through `report-red-runs.yml`. `test/repo/nightly_sql_workflow_test.go` keeps it
in shape.

### The crossservice shard

The compat job's `crossservice` shard runs on the `run` shard's instance again. It runs only
`COMPAT_CROSSSERVICE_TESTS` from `scripts/compat-shards.sh`, which the `run` shard skips: these are
Eventarc's tests, Artifact Registry's cleanup policies, and the tests that need services that no
other shard serves together. Until #2088 it was called `cross`, which was also the name of the
cross-compile job (`make cross`, checks `cross (<target>)`). `test/repo`'s
`TestCompatShardsAreNotNamedAsOtherJobs` now fails if a shard is named like another ci.yml job.

### The gke shard

The compat job's `gke` shard runs on the `served` shard's instance again. It runs only
`COMPAT_GKE_TESTS` from `scripts/compat-shards.sh`, the Kubernetes Engine API's tests (through the
client, gcloud and Terraform), which make kind clusters beside the instance's; the `served` shard
skips them. Together they passed the served suite's 60-minute deadline in main runs 45023269046
and 45047408927: no test hung, and the Kubernetes Engine tests took 2,828 s of it.

### The localaispeech shard

The compat job's `localaispeech` shard runs on the `localai` shard's instance again (the same
`make build-onnx` CLI, models and LiteRT-LM image, which only `localai` saves to the caches). It
runs only `COMPAT_LOCALAI_SPEECH_TESTS`, the local AI tests that transcribe with whisper.cpp
(Speech-to-Text's and BigQuery's `ML.TRANSCRIBE`), which the `localai` shard skips: they took 3,360 s
of its 6,765 s suite in main run 45047408927, before Landing 167's six new local AI tests.

### The local AI shard

The compat job's `localai` shard (#1985) runs the compat tests that need the local generation or
embedding runtime, which skip on every other shard's instance: Vertex AI's generation and
embeddings through the official SDKs, BigQuery's AI and ML functions over them, `ML.ANNOTATE_IMAGE`'s
MediaPipe models, Document AI's Tesseract, Speech-to-Text's whisper.cpp, BigQuery ML's imported
ONNX models, and the console's Studio and Embeddings pages. `scripts/compat-shards.sh` names them
(`COMPAT_LOCALAI_TESTS`, `COMPAT_BROWSER_LOCALAI_SHARD_TESTS`), and the shard runs those alone.
Its instance is the `make build-onnx` CLI, started with `--local-ai-model` and
`--local-ai-embeddings`, and needs, beyond the other shards:

- **The LiteRT-LM image**, built as `make litert-lm` builds it, from `deploy/litert-lm`, and kept
  as a saved image in an Actions cache keyed by that directory's files, so it is built again only
  when they change (a Bazel build from Google's source; 6m17s on 16 cores, so expect several times
  that on the runner's four vCPUs, which the shard's 180-minute job timeout allows for, beside its suite's 120 minutes).
- **The pinned generation model and ONNX Runtime**: `scripts/localai-models.sh <dir>` fetches
  `localai.generationModel` (Gemma 4 E2B's community LiteRT-LM conversion, 2,588,147,712 bytes, at
  the pinned Hugging Face revision; public and ungated, so no sign-in and no token) and, on Linux
  amd64, `localai.onnxRuntime` (`libonnxruntime.so` from Microsoft's PyPI wheel), checks each file's
  size and SHA-256 from `dependencies.json`, and prints `CLOUDBURROW_LOCALAI_MODEL` and
  `ONNXRUNTIME_LIB`. Cached keyed by those pins alone.
- **The instance's own models**, which `up` and the tests fetch into `<state-dir>/models`, each
  verified by its pin in `internal/localai`: EmbeddingGemma's and EmbeddingGemma 2's community ONNX
  conversions, whisper.cpp's, Tesseract's and MediaPipe's. Cached keyed by those files, and saved
  after the suite whatever its result.

The shard is part of the compat job, so it gates `ci-green` and, through main's push run, the
scheduled release, like every other shard. Run it locally with
`scripts/verify-local.sh --shards localai`, after `scripts/localai-models.sh <dir>` and with
`ONNXRUNTIME_LIB` set (on macOS, Homebrew's `onnxruntime`).

### The embedded BigQuery emulator

Every CLI embeds CloudBurrow's build of the BigQuery emulator (#1061,
[third_party/bigquery-emulator](../third_party/bigquery-emulator/PROVENANCE.md)): two Go builds of
about 200 MB, linux/amd64 and linux/arm64. Each check and compat job used to make them itself,
`make build` taking 8 min 28 s and 8 min 31 s on the two Linux check jobs and 12 min 24 s on macOS
(run 36488974273, #1118's landing branch). Since #1087 `ci.yml` builds them once per run:

- The `bigquery-emulator` job restores them from `actions/cache`, under a key of everything
  `tools/bqengine` reads (its Go files, `sources.json`, the wrapper module's `go.mod` and `go.sum`,
  and the patches) and the exact Go version, and runs `make bigquery-binaries`, which keeps a
  binary whose `.inputs` stamp matches and builds any other from source. A run that changes none
  of those inputs builds nothing; one that changes any builds them, here only, and saves them.
  It uploads them as the run's `bigquery-emulator` artifact with a `SHA256SUMS` file, and reports
  the SHA-256 of that file as a job output.
- The `check` jobs (both Go versions, and macOS) and the compat shards download the artifact,
  check the sums file against the job output and each file against the sums, put the files in
  `internal/bigqueryimage/bin/`, and build with `BQENGINE_FLAGS=-prebuilt`, which builds nothing
  and fails unless each binary's stamp names the sources checked out. So a binary built by
  another Go version or on another platform is used, and one built from other sources is not.

Releases do not use the cache. `release.yml` builds the binaries from source once per run, with the
release's Go (`dependencies.json` `toolchain.goRelease`), in its own `bigquery-emulator` job, and
its four `build` jobs check them the same way and embed them with `-prebuilt`: every CLI of a
release embeds the same bytes, built from the pinned, checksummed modules by that run.

## Releasing

A release goes out every one to two days on its own (#1898). `.github/workflows/scheduled-release.yml`
runs four times a day; [`scripts/scheduled-release.sh`](../scripts/scheduled-release.sh) decides,
and the release itself is `release.yml`, the same jobs a pushed tag runs: the CI gate, `make check`,
the builds, macOS signing and notarization, the smoke installs on every platform, promotion to
latest and the Homebrew tap.

**Which commit.** The newest commit on `main` whose own push run of `ci.yml` passed in full: every
job passed or was skipped by design, and the compatibility shards, the check that every compat test
ran in a shard, the integration suite and `ci-green` all passed. A pull request's `ci-green` is not
enough, because pull-request CI runs the fast jobs only, and a push run cancelled by the next push to
`main` proves nothing. `scripts/require-ci-green.sh --main-push <sha>` is the check, and
`release.yml` applies it to every release, a tag pushed by hand included.

**When.** Nothing happens when that commit is already tagged or is not newer than the last release,
or while the last release (its tag's date) is under 24 hours old. From 24 to 48 hours, the commit is
released only if it is `main`'s head, so a release does not go out while newer work is still in CI.
From 48 hours on, the newest fully green commit is released whatever `main`'s head is doing.

**Which version.** While CloudBurrow is before 1.0, a minor bump (`v0.3.0` to `v0.4.0`) when a
feature branch landed since the last tag, else a patch bump (`v0.3.0` to `v0.3.1`). A feature
branch landed when any merge commit in `<last tag>..<commit>` merged a branch named `feat/...`: a
landing's `Merge feat/1799-nl (...) into Landing 149`, a pull request's `Merge pull request #N
from cloudburrow/feat/...`, or `Merge branch 'feat/...'`. The rule reads branch names git records
in every merge, so it needs no API, label or issue lookup; name a branch `fix/...` when it only
corrects behaviour. The major version never moves on its own.

**Release notes.** Nothing is committed to `main`. The notes are generated from the merges since the
last tag (a section per kind for the `feat/` and `fix/` branches each landing merged, then the
landings themselves) and stored as the annotated tag's message. `scripts/release-notes.sh` uses a
tag's `## [X.Y.Z]` section of `CHANGELOG.md` when there is one, and otherwise the tag message, if it
starts with the generated-notes marker. To publish hand-written notes instead, add the section to
`CHANGELOG.md` on `main` first, or tag by hand.

**Starting a release without the schedule.** Actions › Scheduled release › Run workflow:

- with **dry run** ticked (the default), it decides and shows the commit, the version and the notes
  in the run's summary, and tags and releases nothing;
- untick **dry run** to release for real, on the same rules;
- tick **force** to skip the 24 and 48 hour rules (the commit must still have passed `main`'s full CI
  run and be newer than the last release);
- set **version** (`vX.Y.Z`, newer than the last tag) to choose the version, for example `v1.0.0`.

The same from a shell: `gh workflow run scheduled-release.yml -R cloudburrow/cloudburrow -f
dry_run=false -f force=true`.

**Skipping releases.** Set the repository variable `RELEASE_SCHEDULE_PAUSED` to `true` to pause the
schedule (a manual run still works), or disable the workflow with `gh workflow disable
scheduled-release.yml`. A commit that should not be released needs nothing: a newer green commit
replaces it at the next run.

**By hand.** Pushing a `vX.Y.Z` tag still releases, through the same gate: the tagged commit needs
`main`'s full CI run. `release.yml`'s own manual dispatch remains a dry run that builds the
archives and renders a tag's notes, and publishes nothing.

### The vulnerability scanner's mirror and pins

Artifact Registry's vulnerability scanning (#1912) runs Google's OSV-Scanner against snapshots of
the OSV database pinned by object generation and SHA-256 (`internal/osvscan/pins.go`,
`dependencies.json` `scanning`). OSV keeps an old generation for only about a week, so (#1917):

- **Each release mirrors its pins.** `release.yml`'s `osv-mirror` job, after `publish`, runs
  `scripts/osv-mirror.sh stage` (`tools/osvmirror stage`): it downloads the four OSV-Scanner
  binaries and the pinned snapshots of Alpine, Debian, Go, Maven, npm and PyPI, from upstream
  first and then from the five newest earlier releases (the asset names carry the SHA-256, so the
  same name is the same bytes), checks every file against the pin at the release's tag, and
  uploads them (about 600 MB) with `osv-mirror-SHA256SUMS` and `osv-mirror-NOTICE.txt`. Only the
  workflow's `GITHUB_TOKEN`. `cloudburrow prefetch --vulnerability-scanning` tries its own
  release's copy, then the latest release's, then upstream. It does not gate `promote`: a
  release without its mirror still prefetches from upstream while OSV serves its pins, and the
  job failing means the pins are older than OSV's week and no earlier release has them.
- **Ubuntu is not mirrored**: 820 MB, more than twice the six above together, on a release
  every one to two days. [Install](install.md) says what prefetch does without it.
- **The pins move every few days.** `.github/workflows/osv-pins.yml` runs daily; once the pins are
  three days old, `tools/osvmirror bump` reads each archive's live generation from the bucket's
  metadata, downloads and hashes it, and rewrites `pins.go` and `dependencies.json` together; the
  unit tests check they agree; `scripts/osv-mirror.sh propose` pushes the change to a branch
  `auto/osv-pins-<date>` with the workflow's `GITHUB_TOKEN` and opens a pull request. This
  repository does not let GitHub Actions create pull requests ("Allow GitHub Actions to create
  and approve pull requests" is off), so `gh pr create` is refused and the script files (or
  updates) an issue titled `OSV database pins: move to <date>` linking the branch instead. A push
  with `GITHUB_TOKEN` starts no CI run: land the branch like any other, and its landing's CI checks
  it. To move the pins now: Actions > OSV database pins > Run workflow, with `min_age` 0.
- **CI scans for real.** The compatibility job's `run` shard prefetches the scanner and the
  Alpine snapshot into the instance's prefetch cache before `up` (`tools/osvmirror fetch`, about
  67 MB), from an Actions cache keyed by `pins.go`, else the latest release's mirror, else
  upstream, and sets `CLOUDBURROW_TEST_OSV_REQUIRED`, so the scanning compat tests fail rather
  than skip without it.
