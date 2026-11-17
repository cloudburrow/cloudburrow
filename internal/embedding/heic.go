package embedding

import (
	"image"
	"io"

	"github.com/gen2brain/h265/heic"
)

// HEIC and HEIF images (#1724) are decoded with github.com/gen2brain/h265,
// a pure Go HEVC decoder and HEIF container reader (MIT, no dependencies,
// no cgo; docs/upstream-evaluation.md says why it was chosen). The
// container's clean aperture, rotation and mirror (clap, irot, imir) are
// applied, as libheif applies them by default, so the picture is the one a
// viewer shows; an image sequence gives its first frame, as an animated
// WebP does (webpanim.go). A HEIF whose primary item is not HEVC (AV1, as
// in AVIF, or JPEG) is refused by the decoder as unsupported.

// heicOptions are the decode options: transforms applied.
var heicOptions = heic.Options{AutoRotate: true}

func heicConfig(r io.Reader) (image.Config, error) { return heic.DecodeConfig(r) }

func heicDecode(r io.Reader) (image.Image, error) { return heic.Decode(r, heicOptions) }
