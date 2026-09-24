# CloudBurrow in CI

CloudBurrow runs on a CI machine the same way it runs on a laptop: a kind cluster in Docker, with
`cloudburrow up` serving the APIs. Two recipes follow. The first uses the GitHub Action in this
repository, and the second is plain shell that works on any runner with Docker.

> **No release has been cut yet** ([docs/install.md](install.md)). Until v0.1.0 exists, use
> `version: source`, which builds CloudBurrow from the action's checkout and needs Go, or build
> from source in the shell recipe.

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
   against `checksums.txt`, and its build attestation with the job's token. For `source`, it runs
   `go build` on the action's checkout. The binary goes on `PATH`.
2. **Checks for Docker, kind and kubectl**, and names whichever is missing.
3. **Runs `cloudburrow up --detach`**, which returns once the instance is ready, then
   `cloudburrow wait`.
4. **Appends `cloudburrow env --format plain` to `$GITHUB_ENV`.** Nothing needs configuring in
   test code for services that have an emulator variable. For those that don't (Cloud Tasks,
   Secret Manager, BigQuery), see [configuration.md](configuration.md#endpoints-and-sdk-configuration).
5. **Afterwards, always:** it prints `cloudburrow status --format json` and the last 200 log
   lines per container in collapsed groups, then runs `cloudburrow delete`. It checks the cluster
   is gone, and this runs whether the job passed or failed.

Inputs: `version` (`latest`), `services`, `mode` (`ephemeral`), `name` (`ci-<run id>-<job id>`),
`timeout` (`10m`), `github-token`. Outputs: `name`, `bin-dir`.

### Keeping logs from a failed run

The cleanup step runs after every step in the job, so anything it wrote would come too late for an
upload step to see. Collect diagnostics yourself in a step that runs only on failure, before the
job ends:

```yaml
      - name: CloudBurrow diagnostics
        if: failure()
        run: |
          mkdir -p cloudburrow-diagnostics
          cloudburrow status --format json > cloudburrow-diagnostics/status.json || true
          cloudburrow logs --tail 500 > cloudburrow-diagnostics/logs.txt || true
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

`cloudburrow status --format json $FLAGS` gives a script the instance's state, and exits 0 ready,
3 not running or 4 not ready ([configuration.md](configuration.md#status-for-scripts)).

## What this repository runs

`.github/workflows/action-selftest.yml` runs the action with `version: source` and then a Go test
that creates a bucket and publishes to Pub/Sub, using nothing but the exported environment. A
second job fails a step on purpose. A third job reads both jobs' logs and requires the line the
cleanup step prints once `kind get clusters` no longer lists the cluster. That proves the cluster
is deleted even when a test fails.
