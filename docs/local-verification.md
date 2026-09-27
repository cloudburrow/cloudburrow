# Local verification

GitHub's macOS runners cannot run the cluster, so the macOS/arm64 (Docker Desktop) paths are
verified by running CI's compat job on a Mac: `make verify-local` (`scripts/verify-local.sh`,
#705). It runs each shard of ci.yml's compat job the way CI does (`up --detach`, the
environment from `scripts/compat-env.sh`, the compat suite, the acceptance tests and the
restart probes, then `stop` and `delete`), reading what each shard starts and runs from
`scripts/compat-shards.sh`, the file the compat job sources. It needs no CI variable and
leaves a developer's own instance alone: its instances are named `verify-<time>-<shard>`, sit
at `--port-base 41000` in a state directory of their own, and are deleted on exit, together
with any Docker image the run pulled. It writes a dated JSON result.

```sh
make verify-local                                      # every shard
make verify-local VERIFY_ARGS="--shards run,storage"   # a subset
```

## 2026-09-27, macOS/arm64, Docker Desktop

Result: [`local-verification/2026-09-27-darwin-arm64.json`](local-verification/2026-09-27-darwin-arm64.json).

| | |
|---|---|
| Date | 2026-09-27, 16:04 to 16:29 UTC (1507 s) |
| Machine | macOS 27.0 (26A428), arm64 |
| Docker | Docker Desktop, engine 29.8.0, `aarch64`, kernel 7.0.12-linuxkit, 16 CPUs and 39.1 GiB given to the VM |
| CLI | `v0.1.0-156-g41048a1`, commit 41048a1: a working commit of this change on the #765 train, whose scripts differ from the merged ones only in the failure detail the result records |
| Tools | go 1.27.1, kind v0.33.0, kubectl v1.36.1, Node.js v26.9.0, Python 3.14.7, Terraform 1.16.1, gcloud 586.0.0, Chrome 153; no OpenTofu |
| Storage | the builtin storage server (`dev.local/cloudburrow-storage`, linux/arm64), in-cluster |
| Pods reach the CLI | through `host.docker.internal` (192.168.65.254): no relay, as `up` logged in the run and acceptance shards |

**Result: 378 passed, 1 failed.** The one failure is a check, not a test: the run shard's
instance did not come back up after `stop` for its browser suite (below).

| Shard | Passed | Failed | Skipped | What ran |
|---|---|---|---|---|
| storage | 211 | 0 | 117 | compat 155 (111 skipped), Node.js 16, Python 18 (6 skipped), probe setup 4, restart probes 4 + 4, browser 10; the hooks, seed, `logs` and `stop` checks passed |
| served | 53 | 0 | 216 | compat 50 (216 skipped), probe setup 1, restart probes 1 + 1 |
| run | 57 | 1 | 209 | compat 57 (209 skipped), including `TestAPodReachesTheCLIHostedServices`; the browser suite did not run |
| emulators | 43 | 0 | 235 | compat 31 (235 skipped), Node.js 3, probe setup 1, restart probes 4 + 4 |
| acceptance | 14 | 0 | 0 | test/e2e 1, test/k8s 13 (a skip counts as a failure here, as in CI) |

A compat test skips when its shard does not export its service's variable: that is how CI's
shards divide the suite (#565), and most skips in each shard are the other shards' tests. The
Terraform `--binary tofu` tests skipped in storage because OpenTofu is not installed on this
machine; CI installs it.

`TestAPodReachesTheCLIHostedServices` passed through `host.docker.internal`: a pod read a
secret from Secret Manager and enqueued a Cloud Tasks task at
`cloudburrow-host.cloudburrow.svc.cluster.local`, the CLI-hosted services on the Mac's
loopback, which Docker Desktop forwards (#575).

**What failed.** In the run shard, after the compat suite and `stop`, `up --detach` again with
the same flags (for the Cloud KMS and Cloud Run console screens, which only that instance
serves, #700) failed while installing components:

```
component install failed: set the revision progress deadline: kubectl: exit status 1:
Error from server (InternalError): Internal error occurred: failed calling webhook
"config.webhook.serving.knative.dev": failed to call webhook: Post
"https://webhook.knative-serving.svc:443/config-validation?timeout=10s": dial tcp
10.96.87.219:443: connect: connection refused
```

`up` had logged `knative knative-v1.23.0 already installed` and waited for `knative-serving`,
but the restarted cluster's Knative webhook was not yet answering when `up` set the revision
progress deadline. It is reproducible: a second run of the run shard alone, at 16:29 UTC with
the same commit, failed the same way (the webhook at another cluster IP). So on this machine a
stopped Cloud Run instance did not restart. The
storage, served and emulators instances did restart, for their probes. The browser suite's
KMS and Cloud Run tests were not run.

## History

- 2026-09-27: `scripts/verify-local.sh --shards run,storage` on the same Mac, at commit
  a0d0f48 (this change on #762): run 57 passed, storage 207 passed, none failed. The browser
  suite then ran only in storage.
- 2026-09-20 (#25): the Kubernetes architecture was stood up by hand before `cloudburrow up`
  existed: kind `v1.36.4` Ready in 37.6 s, Knative Serving and Kourier v1.23.0, the Pub/Sub
  emulator and fake-gcs-server (removed since, #519), and the acceptance workflow's shape
  driven by official SDKs. That record, with the three things that failed first time, is
  [local-verification.md at 1576229](https://github.com/cloudburrow/cloudburrow/blob/15762298ef4e164275ce769e87d5823649aa0758/docs/local-verification.md).
