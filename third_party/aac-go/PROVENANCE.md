# aac-go, copied

Source: `github.com/arabian9ts/aac-go` v0.1.0 (commit `c4cc4c5fae73af12eb0a54aa745f10d105c825f6`,
tagged 2026-08-11; the Go module proxy's zip, go.sum `h1:e298pHWu+Xcv6VPU2hmS1UTmOq5C9KHD38eQcJR4K8c=`),
Apache License 2.0 (`LICENSE`, `NOTICE`, as upstream ships them). A pure Go AAC-LC encoder and
decoder, standard library only.

CloudBurrow decodes a video's AAC-LC audio track with it for Vertex AI EmbedContent's
`audio_track_extraction` (#1732, `internal/embedding/videoaudio.go`). It was required as a module
until #1739, which copied it here so CloudBurrow owns its fixes: upstream has one release and no
activity since, and the decoder is where HE-AAC (SBR, PS) would be added.

## What is copied

Every non-test Go file of the module root, `adts`, `wav` and `internal/{bits,coder,tables,transform}`,
and `LICENSE` and `NOTICE`. Left out: `cmd/` (its `aacenc` and `aacdec` tools), `scripts/`,
`testdata/`, the tests and `README.md`.

## Changes

The import paths `github.com/arabian9ts/aac-go/...` are rewritten to
`github.com/cloudburrow/cloudburrow/third_party/aac-go/...`. Nothing else is changed.

## Checked

`internal/embedding`'s `TestDecodeVideoAudioTrack`: an AAC-LC track (16 kHz stereo, mid/side, short
blocks) decodes to ffmpeg 9's samples within one 16-bit step a channel, as before the copy.

## Updating

Download the new version with `go mod download`, copy the same files, rewrite the imports, record
the version, sum and commit here and in `dependencies.json`, and run `go test ./internal/embedding/`.
