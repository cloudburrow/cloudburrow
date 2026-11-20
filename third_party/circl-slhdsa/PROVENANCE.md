# CIRCL's SLH-DSA, with a pre-hashed digest constructor

Source: `github.com/cloudflare/circl` v1.6.5 (2026-08-05, the Go module proxy's `v1.6.5.zip`,
go.sum `h1:O64F26HEqNhznd/hrC5KZXVKYuKM2rx4deZDTc4ihQA=`), Copyright (c) 2019 Cloudflare, BSD 3-Clause License (`LICENSE`).

CloudBurrow's Cloud KMS signs with `PQ_SIGN_SLH_DSA_SHA2_128S` and
`PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256` (#1946). Go's standard library has no SLH-DSA (FIPS 205),
and Google's only Go implementation, Tink's, is internal to Tink (the binary links no Tink:
`TestTheBinaryDoesNotLinkTink`). Upstream CIRCL builds a HashSLH-DSA message only from data it
hashes itself (`PreHash`), while Cloud KMS receives the SHA-256 digest the client computed, so
the package is copied here (import path `github.com/cloudburrow/cloudburrow/third_party/circl-slhdsa`)
with one constructor added.

Copied: the non-test `.go` files of `sign/slhdsa`, `internal/conv` and `internal/sha3`; the
`github.com/cloudflare/circl/internal/...` imports are rewritten to this directory's.
Left out: the tests and test data, `sign/slhdsa/scheme.go` (the `circl/sign` adapter) and
`internal/sha3/sha3_s390x.s`.

Changes (`message.go` only):

- added by CloudBurrow: `NewPreHashedDigest(h, digest)`, the `Message` FIPS 205 Algorithm 23 signs
  (OID || PH_M) for a digest already computed with `h`; `TestNewPreHashedDigestMatchesPreHash`
  checks it against `PreHash.BuildMessage`;
- removed: `NewPreHashWithXof` and `BuildMessage`'s XOF case (SHAKE pre-hashing), so the copy
  imports no `github.com/cloudflare/circl/xof`.

Signatures are checked in the compat suite with Tink's own SLH-DSA (pure) and with upstream CIRCL
(pre-hash), `TestKMSPostQuantumSignatures`.
