package main

import (
	"context"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
	"github.com/cloudburrow/cloudburrow/internal/videodecode"
)

// videoDecoder serves multimodalembedding@001's AVI, FLV, MPEG and WMV
// video (#1651) with internal/videodecode: FFmpeg in a pinned container,
// `docker run --network none`, its image built on the first such video,
// never by `up`. The CLI itself links no FFmpeg and stays static.
type videoDecoder struct{ d *videodecode.Docker }

func (v videoDecoder) Source(ctx context.Context) vertexai.ContainerSource { return v.d.Source(ctx) }

// newVideoDecoder writes the image build's start and outcome to out.
func newVideoDecoder(out io.Writer) videoDecoder {
	return videoDecoder{&videodecode.Docker{
		Logf: func(format string, args ...any) { fmt.Fprintf(out, "cloudburrow: "+format+"\n", args...) }}}
}
