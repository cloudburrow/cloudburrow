# The AVIF decoder CloudBurrow vendors

Vertex AI EmbedContent's AVIF images (#1729) are decoded by **libavif 1.4.2 with dav1d 1.5.3,
compiled to WebAssembly by [gen2brain/avif](https://github.com/gen2brain/avif) v0.6.0** and run
on [wazero](https://github.com/tetratelabs/wazero) v1.12.0, a pure Go WebAssembly runtime. The
module's WebAssembly path is copied into [internal/avif](../../internal/avif); the module itself is
not a dependency.

## Why it is vendored, not required

`github.com/gen2brain/avif` first tries a system libavif through
[purego](https://github.com/ebitengine/purego) in every build without `-tags nodynamic`. With
`CGO_ENABLED=0`, purego links its `fakecgo` shim and imports libc dynamically, so a Linux CLI that
requires the module is dynamically linked (docs/upstream-evaluation.md, #1724). The user decided on
2026-11-17 to vendor only the WebAssembly path, without purego, so no build tag is needed and the
CGO_ENABLED=0 Linux CLI stays statically linked; `test/repo`'s `TestLinuxCLILinksNoPurego` checks
it.

## Where each file comes from

From the module zip the Go module proxy serves for `github.com/gen2brain/avif@v0.6.0`
(`h1:/8WSgcU+IEF0jhKYsUZ/mzlziFuTeJFpIKBj2siTQps=`, checked against the checksum database by the
go command), tag v0.6.0, commit `1fd2a9a51895ca03a192dd49917840794c8a77a1` (2026-07-05). SHA-256
of each file, upstream and as committed:

| File in internal/avif | Upstream SHA-256 | Committed SHA-256 | Changed |
|---|---|---|---|
| `avif.go` | `4c058351b9d67b094d5cea395b7e5d375fe4ce3b89736d814729c495aecd4d92` | `5211d811be32260ec6c991308a2bc2646dd9f2c04838d26362074019e36e7462` | the dynamic-library path deleted (below) |
| `avif_wazero.go` | `a60925be429779481cc683b39489194ce6e533d2cf8bde6e1efec3661049df31` | `b02291fe9bcc0bb0f89d42fface8df63fd6130fecf3c53df19e9b9f8d5a59e2a` | its `//go:build !wasm2go` line deleted |
| `isobmff.go` | `b8db4004e6e429f7c75cfd6c8613df8024a4e5ef3d2d3d852dcf4e8a223fd006` | `b8db4004e6e429f7c75cfd6c8613df8024a4e5ef3d2d3d852dcf4e8a223fd006` | no |
| `orientation.go` | `18ebce9fc39f7728193ff9132c4af0f94237bf9323b5295e4c379b60529fee27` | `18ebce9fc39f7728193ff9132c4af0f94237bf9323b5295e4c379b60529fee27` | no |
| `lib/avif.wasm.gz` | `92ffd604d276359c2df829effc615cd9f286c36ece5761644929e4396eb97360` | `92ffd604d276359c2df829effc615cd9f286c36ece5761644929e4396eb97360` | no |
| `LICENSE` | `7ccd0e2f82eac9b87608576a9e559567f1ba049bebe709135eef3a9661bbc96e` | `7ccd0e2f82eac9b87608576a9e559567f1ba049bebe709135eef3a9661bbc96e` | no |
| `lib/LICENSE.libavif` | `10952217a6d404de8bf8a997fbea9b88f682df1fe98cb9b9f467ade641525639` | `10952217a6d404de8bf8a997fbea9b88f682df1fe98cb9b9f467ade641525639` | no |
| `lib/LICENSE.aom` | `4764a286d8b2faeaf42f4418e7d7a28d58fc8fd4d00a3d0a7f44b0a4099de7f2` | `4764a286d8b2faeaf42f4418e7d7a28d58fc8fd4d00a3d0a7f44b0a4099de7f2` | no |

`lib/avif.wasm.gz` decompresses to the module's `lib/avif.wasm` byte for byte (SHA-256
`52181752f8d92a43dfd0ca33179d32fdf7452a669cc1e260f37fabb8319de6bf`, 3,462,314 bytes). It was built
upstream by the module's `lib/Makefile` from `lib/avif.c` (SHA-256
`7e9603ea6d645c64c86857043b3d97102546c7a8a2d701149daec7a96118bfb9`): libavif v1.4.2, dav1d 1.5.3
(decoding), aom v3.14.1 (encoding, unused here) and libyuv (stable branch), with wasi-sdk.
CloudBurrow compiles no C: it embeds the blob as published. `test/repo`'s
`TestVendoredAVIFMatchesProvenance` checks every committed hash above.

Not copied: `avif_dynamic.go` and `purego_*.go` (the system-library loader), `avif_wasm2go.go` and
`libavif.go` (the same blob transpiled to Go, 15.8 MB, behind `-tags wasm2go`), `exif.go`
(`DecodeExif`, unused), `lib/avif.wasm` (the uncompressed blob), the C source and build files, the
tests and their testdata.

### What was deleted

In `avif.go`, only lines: the `if dynamic { ... }` branches of `doDecode`, `Encode` and `EncodeAll`
(which called `decodeDynamic`, `encodeDynamic` and `encodeAnimationDynamic`, defined in the files not
copied), the `else` around `Encode`'s WebAssembly call (its body re-indented by gofmt), and the
exported `Dynamic()`, which reported the loader's error. In `avif_wazero.go`, the build constraint
that left it out of `-tags wasm2go` builds, whose other file is not copied. Nothing else is changed.

## Licences

- gen2brain/avif's Go glue: MIT, [LICENSE](../../internal/avif/LICENSE).
- The WebAssembly blob: libavif (BSD-2-Clause), with the licences of the code it bundles, dav1d's
  BSD-2-Clause among them, in [lib/LICENSE.libavif](../../internal/avif/lib/LICENSE.libavif); aom
  (BSD-2-Clause) in [lib/LICENSE.aom](../../internal/avif/lib/LICENSE.aom). Both as the module ships
  them. The blob also links libyuv (BSD-3-Clause, the LibYuv Project Authors), whose licence text
  the module does not ship.
- wazero, a module dependency (go.mod, dependencies.json): Apache-2.0.

## Updating

Download the new version with `go mod download github.com/gen2brain/avif@vX.Y.Z`, copy the same
files from the module cache, make the same deletions, and record the new hashes here; run
`go test ./internal/embedding/ ./test/repo/`.
