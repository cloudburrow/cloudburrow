package bigqueryfront

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Read streams across restarts, and SplitReadStream (#1460).
//
// Measured first: ReadRows of a stream made before the front last started
// was UNIMPLEMENTED, the front kept its streams in memory; SplitReadStream
// went to the emulator, which fails every call "unimplemented split read
// stream" (server/storage_handler.go, v0.8.1, read in its source).
//
// The front keeps each stream it sees made, with its table, row
// restriction and schema, in readstreams.jsonl in its state directory,
// one line per stream, and reads them back as it starts, leaving out a
// stream whose session has expired (the session's expire_time), so a
// session made before a restart reads on after it.
//
// SplitReadStream splits a stream at a fraction of its rows, as
// https://cloud.google.com/bigquery/docs/reference/storage/rpc/google.cloud.bigquery.storage.v1#splitreadstreamrequest
// documents it (read 2026-10-16): a primary stream of the beginning of the
// original's rows and a remainder stream of the rest, "allocated
// back-to-back in the original ReadStream", so that the original's first
// rows are the primary's and the rest the remainder's; an empty response
// means the stream "can no longer be split" (fewer than two rows). The
// original stream reads on as before. The fraction must be in (0, 1); one
// outside is INVALID_ARGUMENT (wording UNVERIFIED). The rows are those of
// the table at the split, in the order a read gives them.

const splitReadStream = readService + "SplitReadStream"

// readStreamsStateFile is where the front keeps its read streams.
func readStreamsStateFile(dir string) string { return filepath.Join(dir, "readstreams.jsonl") }

// savedReadStream is one line of readStreamsStateFile.
type savedReadStream struct {
	Name        string    `json:"name"`
	Project     string    `json:"project"`
	Dataset     string    `json:"dataset"`
	Table       string    `json:"table"`
	Restriction string    `json:"restriction,omitempty"`
	Arrow       []byte    `json:"arrow,omitempty"`
	Avro        string    `json:"avro,omitempty"`
	Expires     time.Time `json:"expires,omitzero"`
	From        int64     `json:"from,omitempty"`
	To          int64     `json:"to,omitempty"`
}

// span is rows, a read of st's table, cut to st's share of them: all of
// them unless SplitReadStream made st.
func (st *readStream) span(rows []queryRow) []queryRow {
	if !st.split {
		return rows
	}
	from, to := min(st.from, int64(len(rows))), min(st.to, int64(len(rows)))
	return rows[from:to]
}

// keepStreams reads the streams kept at path, and from then on keeps the
// streams made there (above).
func (s *storageRead) keepStreams(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the read streams it kept", path, err)
	}
	now := s.records.nowTime() // the instance's clock, as the engine's expire_time (#1513)
	var kept []savedReadStream
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var sv savedReadStream
		if json.Unmarshal(sc.Bytes(), &sv) != nil || sv.Name == "" {
			continue // a line cut short by a crash
		}
		if !sv.Expires.IsZero() && sv.Expires.Before(now) {
			continue
		}
		kept = append(kept, sv)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range kept {
		s.rememberLocked(sv.Name, &readStream{table: readTable{sv.Project, sv.Dataset, sv.Table}, restriction: sv.Restriction,
			arrowSchema: sv.Arrow, avroSchema: sv.Avro, expires: sv.Expires, split: sv.To > 0, from: sv.From, to: sv.To})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
		return
	}
	// Write the streams kept, without the expired ones, and append from
	// then on.
	var out bytes.Buffer
	for _, name := range s.order {
		out.Write(savedLine(name, s.streams[name]))
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		logf("bigquery front: write %s: %v; the read streams are kept in memory only", path, err)
		return
	}
	s.statePath, s.logf = path, logf
}

// savedLine is the line of readStreamsStateFile of the stream name.
func savedLine(name string, st *readStream) []byte {
	sv := savedReadStream{Name: name, Project: st.table.project, Dataset: st.table.dataset, Table: st.table.table,
		Restriction: st.restriction, Arrow: st.arrowSchema, Avro: st.avroSchema, Expires: st.expires}
	if st.split {
		sv.From, sv.To = st.from, st.to
	}
	b, _ := json.Marshal(sv)
	return append(b, '\n')
}

// saveLocked appends the stream name to the state file, if there is one.
func (s *storageRead) saveLocked(name string, st *readStream) {
	if s.statePath == "" {
		return
	}
	f, err := os.OpenFile(s.statePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(savedLine(name, st))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		s.logf("bigquery front: keep read stream %s: %v", name, err)
	}
}

// splitStream serves SplitReadStream (above).
func (s *storageRead) splitStream(ctx context.Context, ss grpc.ServerStream) error {
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var req storagepb.SplitReadStreamRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode SplitReadStreamRequest: %v", err)
	}
	if req.GetName() == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	if f := req.GetFraction(); !(f > 0 && f < 1) {
		return status.Errorf(codes.InvalidArgument, "fraction %v: give a value in the range (0.0, 1.0)", f)
	}
	st, err := s.streamOf(req.GetName())
	if err != nil {
		return err
	}
	_, columns, err := st.encoder()
	if err != nil {
		return err
	}
	rows, err := s.queryRows(ctx, st, columns)
	if err != nil {
		return err
	}
	from, to := int64(0), int64(len(rows))
	if st.split {
		from, to = min(st.from, to), min(st.to, to)
	}
	at := from + int64(float64(to-from)*req.GetFraction())
	var resp storagepb.SplitReadStreamResponse
	if at > from && at < to {
		prefix := req.GetName()
		if i := strings.LastIndex(prefix, "/"); i >= 0 {
			prefix = prefix[:i+1]
		}
		primary, remainder := *st, *st
		primary.split, primary.from, primary.to = true, from, at
		remainder.split, remainder.from, remainder.to = true, at, to
		pn, rn := prefix+newReadStreamID(), prefix+newReadStreamID()
		s.remember(pn, &primary)
		s.remember(rn, &remainder)
		resp.PrimaryStream = &storagepb.ReadStream{Name: pn}
		resp.RemainderStream = &storagepb.ReadStream{Name: rn}
	}
	b, err := proto.Marshal(&resp)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	out := rawFrame(b)
	return ss.SendMsg(&out)
}

// newReadStreamID is a new stream's last name element.
func newReadStreamID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "cb" + hex.EncodeToString(b)
}
