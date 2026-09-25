# Cloud KMS differential oracle (fakekms)

`make oracle-kms` runs one request sequence against Google's
[fakekms](https://github.com/GoogleCloudPlatform/kms-integrations/tree/de849afa57f6e46c1268fbced15c161c532bff8b/fakekms)
and against CloudBurrow's Cloud KMS, and compares, for every call, the status
code and the response fields, after server-set times and page tokens are
normalised (#419). It ports the pattern of fakekms's contract tests
(`fakekms/contract/contract_test.go:43-103`, one suite against either server)
to a side-by-side comparison. It is not part of `make check`; `.github/workflows/oracle.yml` runs it nightly
and on demand, never on pull requests, so its result cannot block a merge.

It compares the resource RPCs (create, get and list of key rings, keys and
versions, #419) and the version state machine both servers implement,
UpdateCryptoKeyVersion and DestroyCryptoKeyVersion (#420). fakekms does not
implement Encrypt, Decrypt, UpdateCryptoKeyPrimaryVersion or
RestoreCryptoKeyVersion (`fakekms/interceptor.go:42-51`), so they are not
compared. Measured along the way: fakekms refuses `destroy_scheduled_duration`
(so every key it has uses 30 days) and lets UpdateCryptoKeyVersion re-enable a
DESTROY_SCHEDULED version.

**Agreement with fakekms is not an observation of Google, and never removes an
`// unverified:` annotation.** fakekms is a reference and test oracle only
(council report §3 condition 4). Its error codes are not ground truth, and a
divergence is not evidence that CloudBurrow is wrong. Every known divergence is
listed, with a reason and a council-report citation, in `divergences_test.go`;
any other difference fails the run. The oracle is never pointed at live Google.

## What is vendored, and why

- **Source:** `GoogleCloudPlatform/kms-integrations` at commit
  `de849afa57f6e46c1268fbced15c161c532bff8b` (2026-06-17), the `fakekms`
  package's non-test Go files, in `internal/fakekms`. **Licence:** Apache-2.0,
  in `LICENSE.fakekms`; every file keeps its Apache-2.0 header.
- **Why vendored, not a module requirement:** the package imports
  `fakekms/fault/faultpb` and `fault/mathpb`, which upstream generates with
  Bazel and does not commit, so the module cannot be built from the Go proxy
  (council report §6 marked proxy importability UNVERIFIED; this is the
  measurement).
- **This module is separate** (`test/oracle/fakekms/go.mod`). The root module's
  `go.mod` and `go.sum` do not mention it, `go list -deps ./cmd/cloudburrow`
  contains none of it, and nothing here reaches the binary.

Three changes to the vendored files, each marked `Modified by CloudBurrow` in
place:

1. `fakekms.go`: the fault-injection server and interceptor are removed,
   because their generated package is not committed upstream and the oracle
   injects no faults.
2. `key_factory.go`: RSA keys are generated at runtime rather than read from
   pregenerated PEM files, so no private key is committed. Only asymmetric
   purposes use them, and the oracle compares symmetric keys.
3. `proto_allowlist.go`: a map or repeated field of messages is no longer
   walked as a nested message. Upstream called `v.Message()` on it and
   panicked, so a CreateCryptoKey with `labels` crashed the server.

## Adding a case

Add a step to `steps_test.go`. If the servers differ, decide whether
CloudBurrow is wrong (file an issue) or the difference is justified (add it to
`divergences_test.go` with a reason and a citation). Where CloudBurrow and fakekms agree on a
code that is annotated `// unverified:`, **the annotation stays**: agreement
with fakekms says nothing about Google.
