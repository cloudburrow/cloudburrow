package storage

// Appendable objects (#1443), as docs.cloud.google.com/storage/docs/rapid
// describes them: BidiWriteObject with write_object_spec.appendable creates
// an unfinalized object in a Rapid storage (zonal) bucket. It is the live
// generation at once, read and listed like any other with the bytes
// persisted so far, and has no timeFinalized. Its writer appends with
// flushes; a later stream resumes it with append_object_spec, which with
// the write handle continues the same writer and without it takes the
// object over, so the earlier writer's next append is refused. finish_write
// finalizes it, after which nothing appends. Only the gRPC API has these
// calls, so they act on the store directly, not through the JSON API.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// openAppendable makes o, about to be committed in bucket b, an unfinalized
// appendable object.
func openAppendable(b bucketRecord, o *objectRecord) error {
	if !zonal(b) {
		// rapid-bucket: appendable objects are only in Rapid storage
		// buckets. UNVERIFIED: the status and message.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/rapid/rapid-bucket
		return errorf(http.StatusPreconditionFailed, "conditionNotMet",
			"Appendable objects can only be created in a Rapid storage (zonal) bucket; %s is not one.", b.Name)
	}
	o.Unfinalized, o.AppendHandle, o.MD5 = true, newUploadID(), nil
	return nil
}

// statusOf is the gRPC status the JSON API would answer err with.
func statusOf(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := grpcstatus.FromError(err); ok {
		return err
	}
	rec := &grpcRecorder{header: http.Header{}}
	writeError(rec, err)
	return replyError(&jsonReply{status: rec.status, header: rec.header, body: rec.buf.Bytes()})
}

// appendTarget reads the unfinalized generation gen of bucket/name for an
// append by the writer holding handle ("" checks no handle).
func appendTarget(tx Tx, bucket, name string, gen int64, handle string) (objectRecord, error) {
	o, ok, err := getObject(tx, bucket, name)
	switch {
	case err != nil:
		return o, err
	case !ok || o.Generation != gen:
		return o, grpcstatus.Errorf(codes.NotFound, "no live generation %d of %s/%s", gen, bucket, name)
	case !o.Unfinalized:
		return o, grpcstatus.Errorf(codes.FailedPrecondition, "%s/%s generation %d is finalized and cannot be appended to", bucket, name, gen)
	case handle != "" && o.AppendHandle != handle:
		// UNVERIFIED: the status a superseded writer is refused with.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/rapid/rapid-bucket
		return o, grpcstatus.Errorf(codes.FailedPrecondition, "%s/%s generation %d was taken over by another writer", bucket, name, gen)
	}
	return o, nil
}

// appendResume opens an existing appendable object for a writer: with the
// object's write handle it continues that writer, and with none (or
// another) it takes the object over under a new handle.
func (s *Server) appendResume(bucket, name string, gen int64, handle string, metaMatch, metaNotMatch *int64) (objectRecord, error) {
	var o objectRecord
	err := s.meta.Update(func(tx Tx) error {
		var err error
		if o, err = appendTarget(tx, bucket, name, gen, ""); err != nil {
			return err
		}
		if metaMatch != nil && o.Metageneration != *metaMatch || metaNotMatch != nil && o.Metageneration == *metaNotMatch {
			return grpcstatus.Errorf(codes.FailedPrecondition, "the metageneration precondition failed: %s/%s is at metageneration %d", bucket, name, o.Metageneration)
		}
		if handle == o.AppendHandle {
			return nil
		}
		o.AppendHandle = newUploadID()
		return putObject(tx, o)
	})
	return o, err
}

// appendData appends data to the unfinalized object at its end.
func (s *Server) appendData(bucket, name string, gen int64, handle string, data []byte) (objectRecord, error) {
	var o objectRecord
	if err := s.meta.View(func(tx Tx) error {
		var err error
		o, err = appendTarget(tx, bucket, name, gen, handle)
		return err
	}); err != nil {
		return o, err
	}
	if len(data) == 0 {
		return o, nil
	}
	old, err := s.blobs.Open(o.Blob)
	if err != nil {
		return o, err
	}
	blob, err := s.blobs.Write(io.MultiReader(old, bytes.NewReader(data)))
	_ = old.Close()
	if err != nil {
		return o, err
	}
	size := o.Size
	err = s.meta.Update(func(tx Tx) error {
		var err error
		if o, err = appendTarget(tx, bucket, name, gen, handle); err != nil {
			return err
		}
		if o.Size != size {
			return grpcstatus.Errorf(codes.Aborted, "%s/%s changed during the append", bucket, name)
		}
		o.Blob, o.Size, o.CRC32C, o.Updated = blob.ID, blob.Size, blob.CRC32C, s.now()
		return putObject(tx, o)
	})
	return o, err
}

// appendFinalize finalizes the unfinalized object, checking the whole
// object's CRC32C when the writer sent it.
func (s *Server) appendFinalize(bucket, name string, gen int64, handle string, wantCRC *uint32) (objectRecord, error) {
	var o objectRecord
	err := s.meta.Update(func(tx Tx) error {
		var err error
		if o, err = appendTarget(tx, bucket, name, gen, handle); err != nil {
			return err
		}
		if wantCRC != nil && *wantCRC != o.CRC32C {
			return grpcstatus.Errorf(codes.InvalidArgument, "Provided CRC32C %q doesn't match calculated CRC32C %q.", crcBase64(*wantCRC), crcBase64(o.CRC32C))
		}
		now := s.now()
		o.Unfinalized, o.AppendHandle, o.Finalized, o.Updated = false, "", now, now
		if err := putObject(tx, o); err != nil {
			return err
		}
		return emit(tx, objectEvent{Type: EventFinalize, Object: o, Time: now})
	})
	return o, err
}

// appendWriter is one BidiWriteObject stream on an appendable object.
type appendWriter struct {
	g            *grpcStorage
	bucket, name string
	gen          int64
	handle       string
	persisted    int64
	buf          []byte
	final        bool
}

// appendMaxBuffer is how much an append stream holds before it persists
// without a flush.
const appendMaxBuffer = 4 * grpcChunk

// openAppend starts the stream that req, its first message, opens: a new
// appendable object or an existing one. It reports whether to answer at
// once, as a resume or takeover is answered with the object's size.
func (g *grpcStorage) openAppend(ctx context.Context, req *pb.BidiWriteObjectRequest) (*appendWriter, bool, error) {
	if p := req.GetCommonObjectRequestParams(); p.GetEncryptionAlgorithm() != "" || len(p.GetEncryptionKeyBytes()) > 0 {
		return nil, false, grpcstatus.Error(codes.Unimplemented, "customer-supplied encryption keys on appendable objects are not implemented")
	}
	if aos := req.GetAppendObjectSpec(); aos != nil {
		b, err := bucketID(aos.GetBucket())
		if err != nil {
			return nil, false, err
		}
		if aos.GetGeneration() == 0 {
			return nil, false, grpcstatus.Error(codes.InvalidArgument, "append_object_spec.generation is required")
		}
		o, err := g.s.appendResume(b, aos.GetObject(), aos.GetGeneration(), string(aos.GetWriteHandle().GetHandle()),
			aos.IfMetagenerationMatch, aos.IfMetagenerationNotMatch)
		if err != nil {
			return nil, false, statusOf(err)
		}
		return &appendWriter{g: g, bucket: b, name: o.Name, gen: o.Generation, handle: o.AppendHandle, persisted: o.Size}, true, nil
	}
	spec := req.GetWriteObjectSpec()
	r := spec.GetResource()
	b, err := bucketID(r.GetBucket())
	if err != nil {
		return nil, false, err
	}
	if err := validObjectName(r.GetName()); err != nil {
		return nil, false, statusOf(err)
	}
	body, err := objectToJSON(r, nil)
	if err != nil {
		return nil, false, err
	}
	raw, _ := json.Marshal(body)
	meta, err := parseUploadMeta(raw)
	if err != nil {
		return nil, false, statusOf(err)
	}
	meta.Name, meta.PredefinedACL, meta.appendable = r.GetName(), spec.GetPredefinedAcl(), true
	q := url.Values{}
	pre(q, "ifGenerationMatch", spec.IfGenerationMatch)
	pre(q, "ifGenerationNotMatch", spec.IfGenerationNotMatch)
	pre(q, "ifMetagenerationMatch", spec.IfMetagenerationMatch)
	pre(q, "ifMetagenerationNotMatch", spec.IfMetagenerationNotMatch)
	cond, err := parseObjectPreconditions(q, "if")
	if err != nil {
		return nil, false, statusOf(err)
	}
	empty, err := g.s.blobs.Write(bytes.NewReader(nil))
	if err != nil {
		return nil, false, statusOf(err)
	}
	o, err := g.s.finalizeObject(b, meta, empty, cond, nil, nil)
	if err != nil {
		return nil, false, statusOf(err)
	}
	g.s.wakeNotifier()
	return &appendWriter{g: g, bucket: b, name: o.Name, gen: o.Generation, handle: o.AppendHandle}, false, nil
}

// add takes one message: its data, then a flush or the finish.
func (w *appendWriter) add(req *pb.BidiWriteObjectRequest) error {
	if w.final {
		return nil
	}
	if d := req.GetChecksummedData(); d != nil {
		if d.Crc32C != nil && checksummed(d.GetContent()).GetCrc32C() != d.GetCrc32C() {
			return grpcstatus.Errorf(codes.InvalidArgument, "the data at write_offset %d does not match its crc32c", req.GetWriteOffset())
		}
		end := w.persisted + int64(len(w.buf))
		content := d.GetContent()
		switch off := req.GetWriteOffset(); {
		case off > end:
			return grpcstatus.Errorf(codes.OutOfRange, "write_offset %d is past the object's %d byte(s)", off, end)
		case off < end:
			content = content[min(int64(len(content)), end-off):]
		}
		w.buf = append(w.buf, content...)
	}
	if req.GetFlush() || req.GetFinishWrite() || len(w.buf) >= appendMaxBuffer {
		if err := w.persist(); err != nil {
			return err
		}
	}
	if req.GetFinishWrite() {
		var crc *uint32
		if sums := req.GetObjectChecksums(); sums != nil {
			crc = sums.Crc32C
		}
		if _, err := w.g.s.appendFinalize(w.bucket, w.name, w.gen, w.handle, crc); err != nil {
			return statusOf(err)
		}
		w.g.s.wakeNotifier()
		w.final = true
	}
	return nil
}

func (w *appendWriter) persist() error {
	if len(w.buf) == 0 {
		return nil
	}
	o, err := w.g.s.appendData(w.bucket, w.name, w.gen, w.handle, w.buf)
	if err != nil {
		return statusOf(err)
	}
	w.persisted, w.buf = o.Size, nil
	return nil
}

// status is the object as it now stands, with the write handle while it is
// open.
func (w *appendWriter) status(ctx context.Context) (*pb.BidiWriteObjectResponse, error) {
	o, err := w.g.getObject(ctx, w.bucket, w.name, url.Values{"projection": {"full"}, "generation": {strconv.FormatInt(w.gen, 10)}}, nil)
	if err != nil {
		return nil, err
	}
	resp := &pb.BidiWriteObjectResponse{WriteStatus: &pb.BidiWriteObjectResponse_Resource{Resource: o}}
	if !w.final {
		resp.WriteHandle = &pb.BidiWriteHandle{Handle: []byte(w.handle)}
	}
	return resp, nil
}

// bidiAppend serves a BidiWriteObject stream on an appendable object, from
// its first message.
func (g *grpcStorage) bidiAppend(stream pb.Storage_BidiWriteObjectServer, req *pb.BidiWriteObjectRequest) error {
	ctx := stream.Context()
	w, answer, err := g.openAppend(ctx, req)
	if err != nil {
		return err
	}
	for {
		if err := w.add(req); err != nil {
			return err
		}
		if answer || req.GetStateLookup() || req.GetFinishWrite() {
			resp, err := w.status(ctx)
			if err != nil {
				return err
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
		answer = false
		if req, err = stream.Recv(); err != nil {
			if err == io.EOF {
				return w.persist()
			}
			return err
		}
	}
}
