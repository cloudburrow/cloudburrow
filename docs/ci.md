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
          version: v0.1.0            # or latest, or source (needs actions/setup-go)
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
| `up --trust` (#598, #619) | the first release after v0.1.0 | left out. v0.1.0 has no trust gate and reads a discovered `cloudburrow.json` and `.cloudburrow/hooks` without it, which is what `--trust` asks for |
| `--port-base`, from the `port-base` input (#584) | the first release after v0.1.0 | the step fails before `up`, saying the input needs a CLI newer than v0.1.0. v0.1.0 moves ports only one by one, so there is nothing to translate it to |

`version: source` builds the CLI from the action's own checkout, which is the same commit, so it
gets every flag. A CLI whose version cannot be read, such as a local `dev` build, is taken to be
current and gets every flag, with a warning.

Action refs from before #679 always pass `--trust`, which v0.1.0 rejects ("flag provided but not
defined: -trust"). Use such a ref with `version: source` or a release after v0.1.0, or move to a
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
