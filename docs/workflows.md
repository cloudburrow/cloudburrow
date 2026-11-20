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

## Executions survive a restart

An execution that was running when a persistent instance stopped carries
on when the instance starts again, from where it was. Every effect an
execution has is journaled with it as it happens: each `http.*` step's
and connector's result, each `sys.log` and call log entry, each callback
endpoint and the requests it receives, each execution
`experimental.executions` starts, when each sleep ends, `sys.now()` and
`uuid.generate()`. After a restart the workflow runs again from the start
and each journaled effect answers from the journal instead of being done
again, so the execution reaches the point it had reached with the same
variables and goes on from there. A sleep under way sleeps only what is
left of it, and a callback endpoint keeps its URL. An HTTP request that was
in flight when the instance stopped is sent again: effects are at least
once.

## Call logging

Set `callLogLevel` on the workflow (`gcloud workflows deploy
--call-log-level`) or on one execution, which wins:

- `LOG_ALL_CALLS`: each call step's start (`BEGUN`, with its arguments),
  return (`SUCCEEDED`, with the response) and exception
  (`EXCEPTION_RAISED`), and an exception handled inside a called
  subworkflow (`EXCEPTION_HANDLED`);
- `LOG_ERRORS_ONLY`: only the exceptions call steps raise;
- `LOG_NONE`: none.

Only explicit call steps are logged, not calls inside expressions, and
`Authorization` headers are redacted. The entries go to the instance's
Cloud Logging, on the `workflows.googleapis.com/Workflow` resource with the
`workflows.googleapis.com/execution_id` label, as
`google.cloud.workflows.type.EngineCallLog` payloads in the
`workflows.googleapis.com/executions_system_log` log, beside the
`ExecutionsSystemLog` entries of each execution's start and end:

```sh
gcloud logging read 'logName:"executions_system_log" AND labels."workflows.googleapis.com/execution_id"="ID"'
```

`sys.log` entries go to the `Workflows` log.

## Customer-managed encryption keys

With `--services` including `kms`, a workflow deployed with
`cryptoKeyName` (a key in the workflow's region) has its source encrypted
with that key, and records `cryptoKeyVersion`. While the key version is
disabled or destroyed, a new execution is refused ("Key version is not
enabled") and a running one fails with that error; once it is enabled
again, executions run. Changing the key makes a new revision.

## experimental.executions

`experimental.executions.run` starts an execution of another workflow
and returns its result; `experimental.executions.map` starts one per
argument and returns their results in order. Either raises
`ExecutionError` when an execution does not succeed. The connector helper
`googleapis.workflowexecutions.v1.projects.locations.workflows.executions.run`
does as `run` does.

## Eventarc

An Eventarc trigger whose `destination.workflow` names a deployed workflow
starts one execution for each event. So does an Eventarc Advanced pipeline
whose destination is a workflow, for each event published to a message bus
whose enrollment's `celMatch` selects it. The argument is the CloudEvent in
JSON.

## Not done

- The console's source editor has no syntax highlighting and no
  visualization graph, and its form has no key or call log level (#2004).

See [compatibility.md](compatibility.md#workflows--googlecloudworkflowsv1-and-googlecloudworkflowsexecutionsv1)
for each claim's status.
