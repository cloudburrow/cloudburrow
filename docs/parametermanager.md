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
each version's payload and its rendered payload.

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

## Not served

Parameter templates (preview) are `UNIMPLEMENTED`, as are tag bindings on
create. Google's request quotas are not enforced. Per-claim status is in
[compatibility.md](compatibility.md#parameter-manager--googlecloudparametermanagerv1).
