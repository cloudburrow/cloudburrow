package bigqueryfront

// The Storage Write streams the front made, kept in files (#1115).
//
// A stream lives in the front: the emulator's Write API is never called
// (storagewrite.go). In memory alone, a restart of the front's container,
// with the emulator still running and holding every table, lost every
// stream (a later call naming one was NOT_FOUND), the rows appended to a
// PENDING stream and not committed, and those appended to a BUFFERED
// stream and not flushed. So, as the Pub/Sub front keeps its state (#898),
// `cloudburrow up` gives the front a directory on an emptyDir volume of the
// BigQuery pod (the storage-write directory of --state-dir): it outlives a restart of the
// front's container and goes with the pod, as the emulator's tables do, so
// the two never disagree on whether a pod restart kept anything.
//
// Each stream has two files, named by its ID:
//
//   - <id>.json, its state (savedStream), replaced whole by a rename at
//     each change;
//   - <id>.rows, for a PENDING or BUFFERED stream, the rows appended to it,
//     one JSON object a line, line k the stream's row at offset k. Only
//     its first Bytes bytes are the stream's: an append writes its rows
//     there first and then the state that counts them, so an append that
//     failed between the two leaves bytes past Bytes, which the next
//     append writes over and a restore ignores.
//
// An append is answered once both are written. The files are not synced:
// what they guard against is the front's process ending, which the page
// cache outlives; a node's crash takes the pod, and the emulator's
// tables, with it anyway. A table's default stream keeps nothing and is
// not kept. A stream's files go when it is committed (the rows file) or
// dropped from the front's bound on streams (both).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
)

// savedStream is a stream's state file.
type savedStream struct {
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Created   time.Time  `json:"created"`
	Rows      int64      `json:"rows"`
	Flushed   int64      `json:"flushed,omitempty"`
	Bytes     int64      `json:"bytes,omitempty"`
	Finalized bool       `json:"finalized,omitempty"`
	Committed *time.Time `json:"committed,omitempty"`
}

// streamID is the last element of a stream's name.
func streamID(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

// holds reports whether st keeps rows until a commit or a flush.
func (st *writeStream) holds() bool {
	return st.typ == storagepb.WriteStream_PENDING || st.typ == storagepb.WriteStream_BUFFERED
}

// keepStreams restores the streams kept in dir, and from then on keeps
// them there. It must be called before the front serves its first call. A
// stream whose files cannot be read is logged and left out; the rest are
// restored.
func (w *storageWrite) keepStreams(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the Storage Write state directory %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read the Storage Write state directory %s: %w", dir, err)
	}
	var restored []*writeStream
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		st, err := loadStream(dir, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			w.logf("bigquery front: a Storage Write stream kept in %s is unreadable, leaving it out: %v", dir, err)
			continue
		}
		restored = append(restored, st)
	}
	sort.Slice(restored, func(a, b int) bool {
		if !restored[a].created.Equal(restored[b].created) {
			return restored[a].created.Before(restored[b].created)
		}
		return restored[a].name < restored[b].name
	})
	w.mu.Lock()
	w.dir = dir
	w.mu.Unlock()
	for _, st := range restored {
		w.remember(st)
	}
	if len(restored) > 0 {
		w.logf("bigquery front: restored %d Storage Write streams from %s", len(restored), dir)
	}
	return nil
}

// loadStream reads the stream id's files in dir.
func loadStream(dir, id string) (*writeStream, error) {
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var s savedStream
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s.json: %w", id, err)
	}
	t, sid, ok := parseStreamName(s.Name)
	if !ok || sid != id || sid == "_default" {
		return nil, fmt.Errorf("%s.json names the stream %q", id, s.Name)
	}
	typ, ok := storagepb.WriteStream_Type_value[s.Type]
	if !ok {
		return nil, fmt.Errorf("%s.json: the stream type %q", id, s.Type)
	}
	st := &writeStream{name: s.Name, table: t, typ: storagepb.WriteStream_Type(typ), created: s.Created, rows: s.Rows,
		finalized: s.Finalized, size: s.Bytes}
	if s.Committed != nil {
		st.committed = *s.Committed
	}
	if s.Rows < 0 || s.Flushed < 0 || s.Flushed > s.Rows {
		return nil, fmt.Errorf("%s.json: %d rows, %d flushed", id, s.Rows, s.Flushed)
	}
	if !st.holds() || !st.committed.IsZero() || s.Flushed == s.Rows {
		return st, nil
	}
	f, err := os.Open(filepath.Join(dir, id+".rows"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, s.Bytes))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<30)
	var k int64
	for sc.Scan() {
		if k >= s.Flushed && k < s.Rows {
			st.held = append(st.held, json.RawMessage(bytes.Clone(sc.Bytes())))
		}
		k++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s.rows: %w", id, err)
	}
	if k != s.Rows {
		return nil, fmt.Errorf("%s.rows holds %d rows, the stream %d", id, k, s.Rows)
	}
	return st, nil
}

// saveStream writes st's state, when the front keeps its streams; st.mu
// is held. The default stream is not kept.
func (w *storageWrite) saveStream(st *writeStream) error {
	dir := w.stateDir()
	if dir == "" || st.isDef {
		return nil
	}
	s := savedStream{Name: st.name, Type: st.typ.String(), Created: st.created, Rows: st.rows, Finalized: st.finalized, Bytes: st.size}
	if st.holds() {
		s.Flushed = st.rows - int64(len(st.held))
	}
	if !st.committed.IsZero() {
		c := st.committed
		s.Committed = &c
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, streamID(st.name)+".json"), b)
}

// writeHeld writes rows to st's rows file after its first st.size bytes
// and returns the file's new length; st.mu is held.
func (w *storageWrite) writeHeld(st *writeStream, rows []json.RawMessage) (int64, error) {
	dir := w.stateDir()
	if dir == "" {
		return 0, nil
	}
	var b bytes.Buffer
	for _, r := range rows {
		b.Write(r)
		b.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(dir, streamID(st.name)+".rows"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	_, werr := f.WriteAt(b.Bytes(), st.size)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return 0, werr
	}
	return st.size + int64(b.Len()), nil
}

// dropHeld removes st's rows file, once its rows are all in the table.
func (w *storageWrite) dropHeld(st *writeStream) {
	if dir := w.stateDir(); dir != "" {
		if err := os.Remove(filepath.Join(dir, streamID(st.name)+".rows")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.logf("bigquery front: %v", err)
		}
	}
}

// forgetFiles removes the files of the streams named, which the front no
// longer keeps.
func (w *storageWrite) forgetFiles(dir string, names []string) {
	for _, n := range names {
		for _, ext := range []string{".json", ".rows"} {
			if err := os.Remove(filepath.Join(dir, streamID(n)+ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				w.logf("bigquery front: %v", err)
			}
		}
	}
}

func (w *storageWrite) stateDir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dir
}

// writeFileAtomic replaces path with b by a rename, so the file is always
// whole.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}
