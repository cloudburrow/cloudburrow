# Parameter Manager

CloudBurrow serves Parameter Manager (#2054) with
`--services parametermanager`, gRPC and JSON on `--port-parametermanager`
(default `9047`), for global parameters and regional ones
(`locations/{region}`) alike. `cloudburrow gcloud-setup` points
`gcloud parametermanager` at it, and `cloudburrow terraform` sets
`parameter_manager_custom_endpoint` and
`parameter_manager_regional_custom_endpoint` for the
`google_parameter_manager_*` resources. The console's **Parameter Manager**
page, in Secret Manager's menu, creates parameters and versions and shows
each version's payload and its rendered payload; its **Parameter templates**
page creates templates and renders them.

Start it beside Secret Manager, which renders read references from, and
Cloud KMS for customer-managed keys:

```sh
cloudburrow up --services secretmanager,kms,parametermanager
```

## Parameters, versions and rendering

```sh
gcloud parametermanager parameters create app-config --location global --parameter-format JSON
gcloud parametermanager parameters versions create v1 --parameter app-config --location global \
  --payload-data-from-file config.json
gcloud parametermanager parameters versions render v1 --parameter app-config --location global
```

With the official Go client, point
`cloud.google.com/go/parametermanager/apiv1` at
`$CLOUDBURROW_PARAMETERMANAGER_ENDPOINT` with `option.WithEndpoint`,
`option.WithoutAuthentication` and insecure gRPC credentials (or
`NewRESTClient` with an `http://` endpoint).

As Google documents:

- A parameter's format is `UNFORMATTED`, `JSON` or `YAML`, fixed at
  creation; each version's payload is checked against it and is at most
  1 MiB.
- A version's payload cannot change; a version can be disabled, and a
  disabled version is read without its payload and cannot be rendered.
- A parameter that has versions cannot be deleted.
- In a `JSON` or `YAML` payload,
  `__REF__("//secretmanager.googleapis.com/projects/P/secrets/S/versions/V")`
  (or a regional secret's `projects/P/locations/L/secrets/S/versions/V`)
  is replaced by the secret version's value when the version is rendered.
  The secret is read as the parameter's own identity, the principal in its
  `policy_member`; with `--iam-enforcement=on` that principal needs
  `roles/secretmanager.secretAccessor` on the secret.
- `kms_key` names a Cloud KMS key in the parameter's location; the service
  agent `service-NUMBER@gcp-sa-pm.iam.gserviceaccount.com` needs
  `roles/cloudkms.cryptoKeyEncrypterDecrypter` on it. Changing the key
  does not re-encrypt existing versions.

## Templates (preview on Google)

Templates are in the v1 API, as Google serves them, global and regional;
gcloud has them under `gcloud beta parametermanager templates` and
Terraform as google-beta's `google_parameter_manager_template`:

```sh
gcloud beta parametermanager templates create app-blueprint --location global --template-format template-format-yaml
gcloud beta parametermanager templates versions create tv1 --template app-blueprint --location global \
  --payload-data-from-file blueprint.yaml
gcloud beta parametermanager templates versions render tv1 --template app-blueprint --location global \
  --parameter-version projects/PROJECT/locations/global/parameters/app-config/versions/v1
```

As Google documents:

- A template is `TEMPLATE_FORMAT_YAML` (the default) or `TEMPLATE_FORMAT_JSON`,
  fixed at creation; unformatted text is not supported.
- A version's payload is a JSON or YAML object of at most 1 MiB, with only
  direct variables, `{{.key}}` or `{{.key1.key2}}`, at most 64 of them; no
  control flow (`if`, `range`) and no `__REF__` secret references, which
  belong in the parameter version.
- Rendering takes one version of a `JSON` or `YAML` parameter, rendered
  first so its secret references are resolved; a variable's value must not
  be an array or a nested key-value pair, and the output must be a valid
  JSON or YAML object of at most 1 MiB. A disabled template version cannot
  be rendered.

Decided here, where Google's pages say nothing: a variable the parameter
version lacks is `INVALID_ARGUMENT`; the parameter version must be in the
template's project and location; a template with versions cannot be
deleted. The console's **Parameter templates** page (Parameter Manager's
Templates tab) creates templates and versions and renders them.

## Tags

`Parameter.tags` on create binds each tag value to the parameter through
the instance's Resource Manager, at
`//parametermanager.googleapis.com/projects/PROJECT_ID/locations/LOCATION/parameters/ID`,
as Google's "Create and manage tags" documents; `gcloud resource-manager tags
bindings list --parent=...` and `ListEffectiveTags` read them, and deleting
the parameter deletes them. The field is input only: `GetParameter` does
not return it.

## Request quotas

Google's per-project, per-minute quotas are enforced: 1,800 render requests
(`RenderParameterVersion`, `RenderTemplateVersion`), 1,200 read requests
(`Get*`, `List*`) and 1,200 write requests (`Create*`, `Update*`,
`Delete*`). A call past one is `RESOURCE_EXHAUSTED` (HTTP 429) with Google's
"Quota exceeded for quota metric ..." text and a `RATE_LIMIT_EXCEEDED`
`ErrorInfo`. Change them for every project in the configuration file,

```json
{ "requestQuotas": { "parametermanager.googleapis.com/read_requests": 600 } }
```

or for one project while the instance runs, through `PUT /admin/quotas`
([configuration.md](configuration.md#admin-api)) or the console's **Quotas &
System Limits** tab of the Parameter Manager API (Edit quota).

## Not served

The Locations mixin is not charged to a quota. Per-claim status is in
[compatibility.md](compatibility.md#parameter-manager--googlecloudparametermanagerv1).
