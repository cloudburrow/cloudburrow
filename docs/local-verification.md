# Local stand-up verification

Date: 2026-09-20 · Issue: #25 · Machine: Apple M4 Max, 48 GB, macOS (Darwin 27.0.0), arm64 ·
Docker 29.8.0 (16 CPU / 39.1 GiB allocated)

The Kubernetes architecture was stood up by hand before being written down as settled. This
records what actually happened, including the two things that did not work first time.

**Result: the pinned component combination works, and the acceptance workflow shape passes
end to end driven by official Google SDKs.**

Manifests are committed in [`deploy/local/`](../deploy/local/) so this is reproducible.

---

## 1. What was verified

| Step | Result |
|---|---|
| kind cluster at `kindest/node:v1.36.4` | Ready in **37.6 s**; server reports `v1.36.4` |
| Knative Serving v1.23.0 | All 4 deployments Available (`activator`, `autoscaler`, `controller`, `webhook`) |
| Knative Kubernetes version check | Passed — no complaint; v1.36.4 ≥ enforced `v1.34.0` |
| net-kourier v1.23.0 | Gateway Available; exposed on host port 31080 |
| Knative Service (`helloworld-go`) | **Ready in 4.6 s**; `HTTP 200` from the host in 2.5 ms |
| Pub/Sub emulator in-cluster | Running, Service reachable |
| fake-gcs-server in-cluster on a PVC | Running, PVC **Bound**, Service reachable |
| Host → backends (port-forward) | Storage `HTTP 200`; Pub/Sub TCP connect OK |
| **Full acceptance workflow** | **PASS** (§3) |
| Global kubecontext | **Untouched** — still unset before and after |

## 2. Commands

```sh
export KUBECONFIG=./cloudburrow.kubeconfig          # never the developer's default
kind create cluster --config deploy/local/kind-cluster.yaml --kubeconfig "$KUBECONFIG" --wait 180s

B=https://github.com/knative/serving/releases/download/knative-v1.23.0
kubectl apply -f $B/serving-crds.yaml
kubectl apply -f $B/serving-core.yaml
kubectl apply -f https://github.com/knative-extensions/net-kourier/releases/download/knative-v1.23.0/kourier.yaml

kubectl patch configmap/config-network -n knative-serving --type merge \
  -p '{"data":{"ingress-class":"kourier.ingress.networking.knative.dev"}}'
kubectl patch configmap/config-domain -n knative-serving --type merge \
  -p '{"data":{"127.0.0.1.sslip.io":""}}'
kubectl patch service kourier -n kourier-system --type merge \
  -p '{"spec":{"type":"NodePort","ports":[{"name":"http2","port":80,"targetPort":8080,"nodePort":31080,"protocol":"TCP"}]}}'

kubectl apply -f deploy/local/backends.yaml
```

## 3. Acceptance workflow

A worker built locally, loaded into the cluster, deployed as a Knative Service, and triggered
by a Pub/Sub **push** subscription. Every client call used an official Google SDK
(`cloud.google.com/go/storage`, `cloud.google.com/go/pubsub/v2`):

```
1. uploaded input.txt via official storage SDK
2. created topic + push subscription -> http://worker.default.svc.cluster.local
3. published message id=2
4. worker wrote result.txt = "CLOUDBURROW WORKS"

ACCEPTANCE WORKFLOW SHAPE: PASS
```

This is the shape of the workflow #19 must prove as a product feature. It is not #19 itself:
here the topology was applied by hand, not by `cloudburrow up`.

## 4. Measured resource budget

Full stack — kind, Knative Serving, Kourier, both emulator backends, one workload, **19 pods
across 6 namespaces**:

| Measure | Value |
|---|---|
| kind node container resident memory | **1.5 GiB** |
| Aggregate pod CPU requests | **2125 m** |
| Aggregate pod memory requests | **1370 Mi** |

Consistent with Knative's own 3 CPU / 3 GB local guidance. This is the floor a developer
pays, and it is far heavier than the superseded single-process design — an honest cost of
ADR-0005, not a footnote.

---

## 5. Two things that did not work first time

Both are real architectural constraints, now written into
[architecture.md §4.2–4.3](architecture.md) and scheduled in #26. Neither would have been
found by reasoning about the design on paper.

### 5.1 Knative rejects locally-loaded images

A locally built image loaded with `kind load` failed:

```
Revision "worker-00001" failed with message: Unable to fetch image
"cloudburrow-worker:verify": failed to resolve image to digest:
HEAD https://index.docker.io/v2/library/cloudburrow-worker/manifests/verify:
unexpected status code 401 Unauthorized
```

Knative resolves tags to digests by contacting the registry, and a local image has no
registry. The **identical image** succeeded once retagged `dev.local/cloudburrow-worker:verify`
— Knative skips tag resolution for `dev.local/`, `ko.local/` and `kind.local/` prefixes.

CloudBurrow must therefore either enforce such a prefix for locally built workloads or run a
local registry. Cloud Run users supply ordinary image references, so the adapter cannot assume
the developer will do this themselves.

### 5.2 One advertised address cannot serve both host and cluster

The workflow initially failed at the last step: the worker succeeded, but the host could not
read the result. The backend advertised

```
mediaLink: http://0.0.0.0:4443/download/storage/v1/b/probe-bucket/o/result.txt?alt=media
```

The official storage client **follows `mediaLink`** on download, so in-cluster reads worked
while host reads failed against an address that does not resolve there.

This is the host-versus-in-cluster addressing problem in concrete form: a single
`-public-host` value cannot satisfy both audiences. The storage adapter must set the
backend's advertised host per audience, or expose an address that resolves identically inside
and outside the cluster.

### 5.3 A third failure that was mine, not the system's

The first workflow run wrote a *wrong* `result.txt`. The cause was the throwaway worker
reading `/{bucket}/{object}` — an XML-API-shaped path that returned a **bucket listing** — and
dutifully uppercasing that. Corrected to
`/download/storage/v1/b/{bucket}/o/{object}?alt=media`, it passed.

Recorded because the push, the Knative execution and the write-back were all working at the
time, and only the content was wrong. A less careful check would have reported the whole
workflow broken, or — worse — seen `result.txt` exist and called it a pass.

---

## 6. Cleanup

```sh
kind delete cluster --name cloudburrow-verify --kubeconfig "$KUBECONFIG"
```

Only the named cluster is deleted. Nothing outside it is touched.
