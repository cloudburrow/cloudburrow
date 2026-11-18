# Certificate Authority Service

CloudBurrow serves Certificate Authority Service v1 (`privateca.googleapis.com`, #1834) in the CLI process. Google publishes no emulator, so it is built to Google's reference: CA pools, root certificate authorities whose keys are generated locally, and the certificates they sign from a CSR or a `CertificateConfig`. The certificates are real X.509 certificates, signed with Go's `crypto/x509`, so any TLS stack verifies them against the CA.

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

- CA pools (ENTERPRISE and DEVOPS), with their issuance policy: baseline values, allowed key types, issuance modes and maximum lifetime.
- Root CAs: created STAGED, as on Google; enable, disable, delete with the 30-day grace period (or `skip_grace_period`), undelete.
- Certificates from a CSR or a config: get, list, revoke, update labels. A DEVOPS pool does not store them.
- `FetchCaCerts`, operations, locations, and IAM policies on CA pools (stored, not enforced).

Every long-running method answers an operation that is already done. Not served yet, and answered 501: subordinate CAs, certificate templates, CRLs, CA keys in Cloud KMS, and CMEK pools. [compatibility.md](compatibility.md#certificate-authority-service--googlecloudsecurityprivatecav1) has each claim and [coverage/privateca.md](coverage/privateca.md) each method.

## Console

The console's Certificate Authority Service page (`/security/cas`, under Security and identity) lists a project's CA pools in every region and creates them; a pool's page lists its CAs and certificates and its trust anchors, and offers Create CA, Request certificate (from a CSR or from a subject and public key), Edit labels, Delete pool and the pool's Permissions tab. A CA's page offers Enable, Disable, Delete (with or without the grace period) and Undelete; a certificate's page Revoke and Edit labels.

## State

In persistent mode the records, the CAs' private keys among them, are kept in `privateca.json` in the instance directory. They are unencrypted: like every CloudBurrow key, they are not a security boundary ([ADR-0004](adr/0004-local-access-and-no-authentication.md)). `/admin/reset?service=privateca` deletes them, and `project=` one project's.
