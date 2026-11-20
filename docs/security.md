# Local security model

CloudBurrow is an unauthenticated emulator. It stores IAM policies without enforcing them, apart
from the local checks [ADR-0006](adr/0006-iam-policy-surface.md) lists; its credentials authorise nothing, and
its Secret Manager and Cloud KMS are not secret stores ([status.md](status.md)). What it does
defend is the developer's machine: the instance's state, the admin API that wipes it, and the
console's terminal, which is a shell. This page describes who can reach CloudBurrow's ports, what
stops them, and which tests fail if a defence is removed. The decisions behind it are in
[ADR-0004](adr/0004-local-access-and-no-authentication.md). How to report a vulnerability is in
[SECURITY.md](../SECURITY.md).

## Who can reach a CloudBurrow port

| Who | How | What stops them |
|---|---|---|
| Another machine | the network | every listener binds `127.0.0.1`; `--allow-remote` is needed to bind anything else, and `up` warns when it is set |
| A web page, by DNS rebinding | `attacker.example` re-resolves to `127.0.0.1`, so the page is a same-origin peer | the **Host check**: every HTTP listener answers only to an IP literal, `localhost`, `*.localhost` and the names CloudBurrow publishes (421 otherwise) |
| A web page on another site | `fetch("http://127.0.0.1:…", {mode: "no-cors"})` or a form POST | the **Origin check**: a request whose `Origin` is not loopback, the listener's own or allowed is refused (403, no CORS headers) |
| A pod in the cluster | `cloudburrow-host`, `host.docker.internal`, and a Service's own name, such as `bigquery.<namespace>.svc.cluster.local` | it reaches the service APIs by design (#575), never the control port; the admin API needs the instance's token. The fronts in the emulators' pods check Host and Origin as the host's listeners do, and answer to their own Service's names (#1587) |
| Another local user | the loopback ports, and the state directory | the admin token, and an owner-only instance directory |

## The checks, one by one

Each row names the test that fails if the protection is taken away. The review that measured
them on a running instance is #1586.

| Check | Found (2026-10-17) | Protection | Test |
|---|---|---|---|
| **Bind addresses** | every listener of `up`, its kubectl tunnels and the kind node's published ports are on `127.0.0.1` (`lsof -nP -iTCP -sTCP:LISTEN`) | loopback by default; `--allow-remote` opt-in | `cmd/cloudburrow` `up_test.go` refuses `--bind-address 0.0.0.0` without `--allow-remote`, and `storageserver_test.go` a non-loopback `--listen` |
| **DNS rebinding** | Service Usage, Cloud SQL Admin and Backup and DR, three in-process REST APIs, answered `Host: attacker.example` | **fixed**: they are behind `hostguard.Wrap` like every other listener | `test/repo` `TestEveryHTTPListenerIsGuarded` fails for any `http.Server` not wrapped or listed with a reason; `internal/console` and `internal/metadata` rebinding tests |
| **CSRF** | a cross-site `text/plain` POST created a Cloud Tasks queue and a KMS key ring; the console API and the admin API refused it | **fixed**: `hostguard.Wrap` checks `Origin` on every listener it guards (below) | `internal/hostguard` `TestWrapRefusesCrossSiteRequests` |
| **Fronts in the pods** (#1587) | the BigQuery, Bigtable, Pub/Sub and call fronts (Spanner, Firestore, Datastore) serve their Service's port in the emulator's pod with no Host or Origin check of their own, trusting the host's tunnel; but pods reach them by the Service's name, kubectl's own listener reaches them past the tunnel guard, and Bigtable's and Spanner's gRPC tunnels are raw although the Bigtable front serves REST and a reset over HTTP/1.1 there | **fixed**: each front's listener is behind `hostguard.WrapService`, which also answers to the names pods dial its Service by (`bigquery`, `bigquery.<namespace>.svc`, `bigquery.<namespace>.svc.cluster.local`) and to no other Service's. Measured through a raw `kubectl port-forward` to the BigQuery Service: `Host: evil.example` 421, `Origin: https://evil.example` 403, a `text/plain` POST 403; `Host: bigquery.cloudburrow.svc.cluster.local` and `127.0.0.1` 200 | `internal/bigqueryfront` `TestRunGuardsHostAndOrigin`, `internal/bigtablefront` and `internal/pubsubfront` `TestFrontRefusesForeignHostAndOrigin`, `internal/callfront` `TestServerRefusesForeignHostAndOrigin`, `internal/hostguard` `TestWrapServiceAnswersToItsServiceName`; `test/repo` `TestEveryHTTPListenerIsGuarded` no longer lists them |
| **CORS** | the Pub/Sub and Firestore emulators behind the tunnels echoed any `Origin` with `Access-Control-Allow-Credentials: true`, so any page could read and write them | **fixed**: the tunnel guard refuses a foreign `Origin`, preflight included, before the emulator sees it | `internal/netfwd` `TestGuardRefusesAForeignOrigin` |
| **Admin API** | every `/admin` route needs `Authorization: Bearer <token>`; no cookie or query parameter is accepted, so a page cannot send it | per-instance token (#553), and the Origin check | `internal/admin` `TestAdminRoutesRequireTheToken` |
| **Metadata server** | `Metadata-Flavor: Google` is required, as on GCE, except on `/token`, `/certs` and IAM Credentials, which Google's clients call without it | the header, and the Origin check, which also covers `/token` | `internal/metadata` `TestFlavorHeaderIsRequired`, `TestMetadataRefusesCrossSitePages` |
| **Console terminal** | the WebSocket upgrade must carry `Origin` naming exactly the console, from a loopback peer | as found | `internal/console` `TestTerminalUpgradeIsRefusedToAnyOtherOriginOrHost` |
| **File permissions** | the instance directory, `admin-token`, `credentials.json`, `credentials-key.pem`, `kubeconfig`, `up.json` and `up.log` were owner-only; an instance directory that already existed with a looser mode stayed that way | **fixed**: `up` tightens the instance directory to `0700` | `cmd/cloudburrow` `TestTheInstanceDirectoryIsMadeOwnerOnly`; `internal/metadata` credentials mode test |
| **Logs and `diagnose`** | no admin token, access token or key in `up.log` or a bundle; the redaction missed a quoted JSON value (`"admin_token": "…"`), a bare `Bearer …` and the body of a PEM key (it cut only the `BEGIN` line) | **fixed** in `console.Redact`, which the console's log views and every `diagnose` file go through | `internal/console` `TestCredentialsAreRedactedBeforeStorage` |

## The Origin check

`internal/hostguard` (`crosssite.go`) serves a request carrying `Origin` only when the origin is:

- **loopback**: `http://` or `https://` at `127.0.0.0/8`, `[::1]`, `localhost` or a name under
  `.localhost`, on any port. A page served from this machine is code the developer chose to run,
  such as a web app under development using the Firestore or Pub/Sub emulator;
- **the listener's own**, the scheme and `Host` the request came to, which the Host check has
  already vouched for: the console's own page, also under `--allow-remote`;
- **allowed** with `up --cors-allow-origin` (config `storage.corsAllowOrigins`). The flag first
  opened Cloud Storage (#677); it now admits the origin on every listener.

A request with no `Origin` is served: the SDKs, `gcloud`, Terraform and `curl` send none, and a
browser sends one on every cross-origin request except a GET or HEAD it makes without CORS (an
image, a script, a link), whose answer the page cannot read. An unsafe method with no `Origin`
but with `Sec-Fetch-Site: cross-site` or `same-site` is refused too. Cleartext HTTP/2, which is
how gRPC arrives and which no browser can send, is not checked, as for the Host check.

The builtin Cloud Storage server checks `Origin` itself, because its CORS answers depend on it
(#677), so it is wrapped with `hostguard.WrapHost`. The console API adds its own stricter check:
`Sec-Fetch-Site` must be `same-origin`, `none` or absent, and `Origin` must be the console's own, so
another loopback page cannot drive it either.

## Nothing reaches Google

Everything runs locally: a tool pointed at an instance must never send a call to a real Google
API, even for an API the instance does not serve (#1975). A tool configuration that leaves an
endpoint unset falls back to that API's `googleapis.com` default, so every configuration
CloudBurrow writes sets **every** endpoint setting the pinned tool has: the served ones at the
instance, every other one at a local refusal, `/_cloudburrow/unserved/<setting>/` on the Resource
Manager port, which every instance serves. It answers any request with `400 FAILED_PRECONDITION`
(no tool retries it) and the message "CloudBurrow refused this request locally: <setting> is not
served by this instance ... nothing was sent to Google".

| Configuration | What it sets |
|---|---|
| `cloudburrow terraform` | All 182 `*_custom_endpoint` arguments of hashicorp/google 8.4.0 and all 196 of google-beta 8.4.0: the local ones and the refusals the provider reads no variable for in the provider block, every other refusal as its `GOOGLE_*_CUSTOM_ENDPOINT` variable for the run. A module that uses `google-beta` gets a `google-beta` block too (before #1975 it got none, so its resources went to Google). The unserved-resource guard (`unpointedResources`) still refuses, before Terraform runs, the families and data sources the provider sends to a fixed Google host whatever the endpoints say. |
| `cloudburrow env --format terraform` | The same, as variables. Four arguments read no variable (`billing_`, `cloud_resource_manager_`, `core_billing_`, and google-beta's `runtimeconfig_custom_endpoint`), so with plain `terraform` and this format they keep their Google default; use `cloudburrow terraform`. |
| `cloudburrow gcloud-setup` | All 202 `api_endpoint_overrides` properties of gcloud 593.0.0, and `auth/token_host` at the instance's token endpoint (gcloud's default is `oauth2.googleapis.com`). |
| The compat harness | `gcloudRestrictConfig` replaces an override the harness does not vouch for with the refusal; it used to drop it, leaving gcloud at Google behind only the egress block. Every tool run is under `noGoogleEgress` as well. |

The inventories are snapshots of the pinned versions (`internal/unserved`, regenerated by
`scripts/endpoint-inventory.sh`). `TestEndpointInventoriesMatchThePins` (test/repo) fails when a
pin in `dependencies.json` moves without them, and the compat test
`TestPinnedToolsGainNoUnmappedEndpoint` reads the real provider schema and gcloud's
`api_endpoint_overrides` section and fails when either has a setting the inventory lacks.
`TestTerraformRefusesAnUnservedResourceLocally` and `TestGcloudRefusesAnUnservedAPILocally` apply
a `google_workflows_workflow` and run `gcloud workflows list` with egress to Google blocked, and
see the refusal.

What no configuration can redirect, and so only an egress block (a proxy or firewall) stops:

- **gcloud APIs with no override.** gcloud 593.0.0 has clients for 21 APIs that have no
  `api_endpoint_overrides` property (`admin`, `agenticapplications`, `artifactscanguard`,
  `bigquerydatatransfer`, `billingbudgets`, `blockchainnodeengine`,
  `blockchainvalidatormanager`, `connectors`, `dataform`, `dialogflow`, `dlp`,
  `essentialcontacts`, `gsuiteaddons`, `hstprof`, `microservices`, `serviceconsumermanagement`,
  `servicenetworking`, `telemetrymanagement`, `translate`, `videointelligence`, `websecurityscanner`): gcloud resolves
  their endpoint with no property to read. `gcloud-setup` names them in the file and on stderr.
- **`gcloud transfer authorize`** asks for the client by its real name, `storagetransfer`, not
  the alias `transfer` the property is named for (`surface/transfer/authorize.py`), so it calls
  `storagetransfer.googleapis.com` whatever `transfer` says. Every other `gcloud transfer`
  command asks for `transfer` and honours it. There is no `storagetransfer` property: a
  configuration that writes one is ignored.
- **A newer tool than the pin.** A provider or gcloud release newer than the inventory may have
  endpoint settings it does not name, which stay at their default. Older providers ignore the
  variables they do not have.
- **Client libraries.** `cloudburrow env` exports `*_EMULATOR_HOST` for the emulators and
  `CLOUDBURROW_*_ENDPOINT` names that no library reads; a library with no endpoint variable of
  its own takes its endpoint only from client options (`option.WithEndpoint`), so for any API
  the code does not configure it calls Google. The environment alone cannot redirect it. `env`
  also exports `CLOUDSDK_API_ENDPOINT_OVERRIDES_*` only for the served APIs, and so does the GitHub
  action, which exports `env --format plain`: use `gcloud-setup` for gcloud, which covers the rest.
- **Hard-coded Google URLs in the provider**, which `unpointedResources` refuses by resource
  type and `--intercept-hardcoded` serves locally (#1578).

## What remains

- **Any page served from a loopback origin is trusted.** Another local dev server, or anything
  the developer serves on `localhost`, can call every service API and, where an emulator sends
  CORS headers, read the answers. That is the intended boundary: code the developer chose to run
  locally. The console API and the admin API are not open to it.
- **The upstream emulators' CORS headers are passed through for an allowed origin**, including
  the Pub/Sub and Firestore emulators' `Access-Control-Allow-Credentials: true`. CloudBurrow sets
  no cookies, so the header grants nothing, and the Firebase web SDK may depend on it.
- **Another local user** can reach the loopback ports as a browser page cannot: the service APIs
  are unauthenticated by design (ADR-0004). The admin API's token and the owner-only instance
  directory keep that user from wiping, exporting or faulting the instance, and from reading its
  keys. The state root (`~/.cloudburrow`) may stay `0755`; it lists instance names only.
- **The gaps ADR-0004 records**: kubectl's own listener behind a guarded tunnel (where a front
  serves the port, BigQuery's, Bigtable's, Pub/Sub's, Spanner's, Firestore's and Datastore's, the
  front's own check now covers it, #1587), multicast DNS on
  the local network for an allowlisted `.local` name, and a pod that writes a loopback `Host` by
  hand to get past the Cloud Build host-steps check.
