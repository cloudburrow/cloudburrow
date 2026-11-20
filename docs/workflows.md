# Workflows

CloudBurrow serves Workflows and Workflow Executions (#1986) with
`--services workflows`, gRPC and JSON on `--port-workflows` (default `9038`).
`cloudburrow gcloud-setup` points `gcloud workflows` at it, and
`cloudburrow terraform` sets `workflows_custom_endpoint` for
`google_workflows_workflow`. The console's **Workflows** page lists the
workflows, deploys a source, runs it, and shows each execution's input,
output and steps.

## Deploy and run

```sh
gcloud workflows deploy hello --source workflow.yaml --location us-central1
gcloud workflows run hello --data '{"name":"burrow"}' --location us-central1
```

With the official Go clients, point `cloud.google.com/go/workflows/apiv1`
and `cloud.google.com/go/workflows/executions/apiv1` at
`$CLOUDBURROW_WORKFLOWS_ENDPOINT` with `option.WithEndpoint` and
`option.WithoutAuthentication`.

## What a workflow can reach: local only

CloudBurrow runs locally, and an execution never sends a request off the
machine.

- An `http.*` step to a loopback address or a `.localhost` name is sent. A
  Cloud Run service's URL is one of these, and is sent through the
  instance's ingress.
- A `googleapis.com` URL of an API this instance serves is sent to that API's
  local endpoint.
- **Any other URL is refused locally, before anything is sent.** This
  includes a `googleapis.com` API the instance does not serve. The step
  raises `ConnectionFailedError` with a message that names the refusal. The
  HTTP client dials loopback only and ignores proxy settings, so a redirect
  cannot leave the machine either.
- A connector (`googleapis.storage.v1.objects.insert`,
  `googleapis.pubsub.v1.projects.topics.publish` and the others) calls the
  local endpoint of its API. If the instance does not serve that API, the
  connector raises `HttpError` 403 `PERMISSION_DENIED` with the reason
  `SERVICE_DISABLED`, and the message says the API is not served here.
- `auth: OIDC` and `OAuth2` tokens are minted by the instance's own key.

## Errors, retries and limits

The interpreter follows Google's syntax reference:

- The error tags are `HttpError`, `ConnectionError`,
  `ConnectionFailedError`, `TimeoutError`, `ValueError`, `KeyError`,
  `IndexError`, `TypeError`, `ZeroDivisionError`, `RecursionError`,
  `ResourceLimitError`, `SystemError` and `UnhandledBranchError`.
- The retry policies are `http.default_retry` (with its predicate and
  backoff), `http.default_retry_non_idempotent`, `retry.always` and
  `retry.never`.
- The documented limits are enforced: 100,000 steps, call depth 20,
  10 branches, 20 concurrent branches, a 128 KB source, 512 KB variables,
  2 MB responses and 400-character expressions.

An execution that was running when the instance stopped is marked FAILED
when the instance next starts: executions do not resume.

## Eventarc

An Eventarc trigger whose `destination.workflow` names a deployed workflow
starts one execution for each event. The argument is the CloudEvent in JSON.

## Not served

- `experimental.executions.*`
- customer-managed encryption keys
- Eventarc Advanced pipelines
- call log levels beyond being recorded

See [compatibility.md](compatibility.md#workflows--googlecloudworkflowsv1-and-googlecloudworkflowsexecutionsv1)
for each claim's status.
