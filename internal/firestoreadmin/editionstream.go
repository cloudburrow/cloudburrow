package firestoreadmin

// The Write stream on Standard databases (#1738).
//
// The Firebase client SDKs write through the Write stream, a bidirectional
// gRPC stream whose first request names the database and whose later ones
// carry the writes, not through Commit. The guard (edition.go) claims each
// such stream and passes it on to the emulator message by message, both
// ways, with the client's metadata, headers and trailers; a request whose
// writes carry an Enterprise-only value, on a Standard database, ends the
// stream with the refusal Commit gets (INVALID_ARGUMENT, "Value type INT is
// not allowed"), and the emulator never sees it, so none of its writes
// apply, as a Commit's do not. Every other stream goes on unchanged.
//
// The Listen stream carries queries alone, which Standard answers (a filter
// on such a value matches nothing), so it is not claimed.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/metadata"
)

// writeStreamPath is the Write stream's gRPC path.
const writeStreamPath = firestoreService + "Write"

// claimsWriteStream reports whether r is a Write stream over gRPC, which the
// guard passes on itself to see its writes.
func claimsWriteStream(r *http.Request) bool {
	return r.URL.Path == writeStreamPath && r.ProtoMajor == 2 &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// forwardedMetadata is the client's metadata the emulator is sent: all but
// the transport's own.
func forwardedMetadata(ctx context.Context) metadata.MD {
	in, _ := metadata.FromIncomingContext(ctx)
	out := metadata.MD{}
	for k, v := range in {
		switch {
		case strings.HasPrefix(k, ":"), strings.HasPrefix(k, "grpc-"),
			k == "content-type", k == "user-agent", k == "te", k == "host":
			continue
		}
		out[k] = v
	}
	return out
}

// Write passes a Write stream on to the emulator, refusing a Standard
// database's Enterprise-only values.
func (e editionRefusals) Write(srv firestorepb.Firestore_WriteServer) error {
	c, err := e.g.a.docs()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(srv.Context())
	defer cancel()
	up, err := c.Write(metadata.NewOutgoingContext(ctx, forwardedMetadata(srv.Context())))
	if err != nil {
		return err
	}

	// The emulator's answers go back as they come.
	down := make(chan error, 1)
	go func() {
		down <- func() error {
			if md, err := up.Header(); err == nil && len(md) > 0 {
				if err := srv.SendHeader(md); err != nil {
					return err
				}
			}
			for {
				resp, err := up.Recv()
				if err != nil {
					srv.SetTrailer(up.Trailer())
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				if err := srv.Send(resp); err != nil {
					return err
				}
			}
		}()
	}()

	// The client's requests go on, each checked first.
	upErr := make(chan error, 1)
	go func() {
		standard := false
		for first := true; ; first = false {
			req, err := srv.Recv()
			if errors.Is(err, io.EOF) {
				upErr <- up.CloseSend()
				return
			}
			if err != nil {
				upErr <- err
				return
			}
			if first {
				standard = e.g.standard(databaseOf(req.GetDatabase()))
			}
			if standard {
				if err := checkStandardValues(req.ProtoReflect()); err != nil {
					upErr <- err
					return
				}
			}
			if err := up.Send(req); err != nil {
				// The emulator ended the stream; its status comes down.
				upErr <- nil
				return
			}
		}
	}()

	for {
		select {
		case err := <-down:
			return err
		case err := <-upErr:
			if err != nil {
				return err // a refusal, or the client went away
			}
			upErr = nil // the client is done sending; wait for the emulator
		}
	}
}
