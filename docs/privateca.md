# Certificate Authority Service

CloudBurrow serves Certificate Authority Service v1 (`privateca.googleapis.com`, #1834) in the CLI process. Google publishes no emulator, so it is built to Google's reference: CA pools, root and subordinate certificate authorities whose keys are generated locally, certificate templates, CRLs, and the certificates the CAs sign from a CSR or a `CertificateConfig`. The certificates are real X.509 certificates, signed with Go's `crypto/x509`, so any TLS stack verifies them against the CA.

```sh
cloudburrow up --services privateca
eval "$(cloudburrow env)"
```

The API is gRPC and JSON on one port, `--port-privateca` (default `9028`). `env` exports `CLOUDBURROW_PRIVATECA_ENDPOINT`; pass it to the official client:

```go
c, err := privateca.NewCertificateAuthorityClient(ctx,
	option.WithEndpoint(os.Getenv("CLOUDBURROW_PRIVATECA_ENDPOINT")),
	option.WithoutAuthentication(),
	option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

or `NewCertificateAuthorityRESTClient` with `http://` before the address.

## What is served

- CA pools (ENTERPRISE and DEVOPS), with their issuance policy: baseline values, allowed key types, issuance modes and maximum lifetime; and their publishing options.
- Root CAs: created STAGED, as on Google; enable, disable, delete with the 30-day grace period (or `skip_grace_period`), undelete.
- Subordinate CAs: created AWAITING_USER_ACTIVATION; `FetchCertificateAuthorityCsr` gives the CSR, and `ActivateCertificateAuthority` installs the certificate an issuer signed, a CA of this service (`subordinate_config.certificate_authority`, as gcloud and Terraform do it) or a third party (`pem_issuer_chain`). The certificate must hold the CA's key and verify against the chain.
- Certificate templates: predefined values, identity constraints and maximum lifetime applied to `CreateCertificate`, and IAM policies on them.
- CRLs: each CA's CRL is regenerated, signed by its key, whenever it changes state or a certificate it issued is revoked; get, list and update its labels, and IAM policies on it.
- Published content: with the pool's `publish_ca_cert` and `publish_crl`, a CA's `access_urls` point at this port (`/privateca-content/{ca}/ca.crt` and `crl.crl`), which serves them, and the certificates the CA issues name them in their AIA and CRL distribution point extensions. Google publishes to a Cloud Storage bucket; nothing is written to Cloud Storage here.
- Certificates from a CSR or a config: get, list, revoke, update labels. A DEVOPS pool does not store them.
- `FetchCaCerts`, operations, locations, and IAM policies on CA pools (stored, not enforced).

Every long-running method answers an operation that is already done. A CA's key can be a Cloud KMS CryptoKeyVersion of this instance (`key_spec.cloud_kms_key_version`, signing in process); a pool's `encryption_spec` (CMEK) keeps its certificates encrypted at rest with a Cloud KMS key; a CA's `gcs_bucket` publishes its CA certificate and CRLs to this instance's Cloud Storage. `subject_mode` `RDN_SEQUENCE`, `subject.rdn_sequence`, `requested_not_before_time`, `passthrough_extensions` and CEL `identity_constraints` (Google's cel-go) are served; `REFLECTED_SPIFFE` answers Google's "caller does not have a SPIFFE identity" error, as no local caller has one. [compatibility.md](compatibility.md#certificate-authority-service--googlecloudsecurityprivatecav1) has each claim and [coverage/privateca.md](coverage/privateca.md) each method.

## gcloud and Terraform

`cloudburrow gcloud-setup` writes the `privateca` override, so `gcloud privateca` pools, roots, subordinates, templates and certificates reach this instance (`TestGcloudPrivateCA`). `cloudburrow terraform` sets `privateca_custom_endpoint` for the `google_privateca_*` resources (`TestTerraformPrivateCA`).

## Cloud SQL's customer-managed CAs

A Cloud SQL instance with `serverCaMode` `CUSTOMER_MANAGED_CAS_CA` and `serverCaPool` naming a pool
of this service, in the instance's region, has the pool issue its server certificates (#1834): each
is a configuration-based `certificates.create` with an EC P-384 key and the instance's DNS name, so
the pool must allow configuration-based requests and DNS names in SANs and have an enabled CA, and
the certificates (`cloudsql-INSTANCE-...`) are listed in an Enterprise pool. Run both services:
`cloudburrow up --services cloudsql,privateca`. The service agent's
`roles/privateca.certificateRequester` on the pool is not checked. See
[cloudsql.md](cloudsql.md#client-certificates-tiers-and-flags-1380).

## Console

The console's Certificate Authority Service page (`/security/cas`, under Security and identity) lists a project's CA pools in every region and creates them; a pool's page lists its CAs and certificates and its trust anchors, and offers Create CA, Request certificate (from a CSR or from a subject and public key), Edit labels, Delete pool and the pool's Permissions tab. Create CA makes a subordinate too, signed and activated by an issuing CA of this instance, or awaiting a third-party issuer, its page then showing its CSR and offering Activate. A CA's page offers Enable, Disable, Delete (with or without the grace period) and Undelete, and lists its revocation lists; a certificate's page Revoke and Edit labels. The Certificate templates page creates, edits and deletes templates, which Request certificate takes.

## State

In persistent mode the records, the CAs' private keys among them, are kept in `privateca.json` in the instance directory. They are unencrypted: like every CloudBurrow key, they are not a security boundary ([ADR-0004](adr/0004-local-access-and-no-authentication.md)). `/admin/reset?service=privateca` deletes them, and `project=` one project's.
