package datastoreversions

// The log on disk: an append-only file of records in the front's state
// directory, each a google.datastore.v1 Entity message (its properties the
// record's fields) behind its length. The first record names the log's
// generation; a version record holds a key's version and, the first time,
// its base. The file is rewritten from the log in memory when it has grown
// to twice the log's size (pruned versions, dropped databases), so it is
// bounded as the log is.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FileName is the log's file in the state directory.
const FileName = "datastore-versions.log"

// maxRecord bounds one record: an entity is at most 1 MiB, and a version
// record holds two.
const maxRecord = 4 << 20

type store struct {
	path string
	f    *os.File
	w    *bufio.Writer
	size int64
	logf func(string, ...any)
}

func str(s string) *pb.Value { return &pb.Value{ValueType: &pb.Value_StringValue{StringValue: s}} }
func num(n int64) *pb.Value  { return &pb.Value{ValueType: &pb.Value_IntegerValue{IntegerValue: n}} }
func ts(t time.Time) *pb.Value {
	return &pb.Value{ValueType: &pb.Value_TimestampValue{TimestampValue: timestamppb.New(t)}}
}
func ent(e *pb.Entity) *pb.Value {
	if e == nil {
		return &pb.Value{ValueType: &pb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}
	}
	return &pb.Value{ValueType: &pb.Value_EntityValue{EntityValue: e}}
}
func boolean(b bool) *pb.Value { return &pb.Value{ValueType: &pb.Value_BooleanValue{BooleanValue: b}} }

func newRecord(op string, props map[string]*pb.Value) *pb.Entity {
	props["op"] = str(op)
	return &pb.Entity{Properties: props}
}

func genRecord(l *Log) *pb.Entity {
	return newRecord("gen", map[string]*pb.Value{"gen": str(l.gen), "since": ts(l.since)})
}

func sinceRecord(l *Log) *pb.Entity {
	var dbs, projects []*pb.Value
	for db, t := range l.sinceDB {
		dbs = append(dbs, ent(&pb.Entity{Properties: map[string]*pb.Value{"name": str(db), "at": ts(t)}}))
	}
	for p, t := range l.sinceProject {
		projects = append(projects, ent(&pb.Entity{Properties: map[string]*pb.Value{"name": str(p), "at": ts(t)}}))
	}
	return newRecord("since", map[string]*pb.Value{
		"dbs":      {ValueType: &pb.Value_ArrayValue{ArrayValue: &pb.ArrayValue{Values: dbs}}},
		"projects": {ValueType: &pb.Value_ArrayValue{ArrayValue: &pb.ArrayValue{Values: projects}}},
	})
}

func versionRecord(r record) *pb.Entity {
	return newRecord("v", map[string]*pb.Value{
		"db": str(r.db), "key": {ValueType: &pb.Value_KeyValue{KeyValue: r.key}},
		"at": ts(r.at), "ver": num(r.ver), "entity": ent(r.entity),
		"hasBase": boolean(r.hasBase), "base": ent(r.base), "baseVer": num(r.baseVer),
	})
}

func dropRecord(db string) *pb.Entity { return newRecord("drop", map[string]*pb.Value{"db": str(db)}) }

func snapRecord(op, project, db string) *pb.Entity {
	return newRecord(op, map[string]*pb.Value{"project": str(project), "db": str(db)})
}

func entityOf(v *pb.Value) *pb.Entity {
	if e, ok := v.GetValueType().(*pb.Value_EntityValue); ok {
		return e.EntityValue
	}
	return nil
}

// readRecord is the record a version record holds.
func readRecord(e *pb.Entity) record {
	p := e.GetProperties()
	return record{db: p["db"].GetStringValue(), key: p["key"].GetKeyValue(), at: p["at"].GetTimestampValue().AsTime(),
		ver: p["ver"].GetIntegerValue(), entity: entityOf(p["entity"]),
		hasBase: p["hasBase"].GetBooleanValue(), base: entityOf(p["base"]), baseVer: p["baseVer"].GetIntegerValue()}
}

// openStore opens dir's log file and loads it into l, returning the
// generation it names ("" for none) and the snapshots it left open.
func openStore(dir string, l *Log) (*store, string, [][2]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", nil, err
	}
	s := &store{path: filepath.Join(dir, FileName), logf: l.logf}
	gen, snaps, good, err := s.load(l)
	if err != nil {
		return nil, "", nil, err
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, "", nil, err
	}
	// A record cut short by a crash is dropped.
	if err := f.Truncate(good); err != nil {
		_ = f.Close()
		return nil, "", nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, "", nil, err
	}
	s.f, s.w, s.size = f, bufio.NewWriter(f), good
	return s, gen, snaps, nil
}

// load reads the file into l, answering its generation, its open
// snapshots and the length of its whole records.
func (s *store) load(l *Log) (string, [][2]string, int64, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, 0, nil
	}
	if err != nil {
		return "", nil, 0, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	var gen string
	var good int64
	open := map[[2]string]bool{}
	for {
		n, err := binary.ReadUvarint(r)
		if err != nil || n > maxRecord {
			break
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			break
		}
		e := &pb.Entity{}
		if proto.Unmarshal(b, e) != nil {
			break
		}
		good += int64(n) + int64(uvarintLen(n))
		p := e.GetProperties()
		switch p["op"].GetStringValue() {
		case "gen":
			gen = p["gen"].GetStringValue()
			l.gen, l.since = gen, p["since"].GetTimestampValue().AsTime()
			l.dbs, l.bytes = map[string]*dbLog{}, 0
		case "since":
			l.sinceDB, l.sinceProject = map[string]time.Time{}, map[string]time.Time{}
			for _, v := range p["dbs"].GetArrayValue().GetValues() {
				q := entityOf(v).GetProperties()
				l.sinceDB[q["name"].GetStringValue()] = q["at"].GetTimestampValue().AsTime()
			}
			for _, v := range p["projects"].GetArrayValue().GetValues() {
				q := entityOf(v).GetProperties()
				l.sinceProject[q["name"].GetStringValue()] = q["at"].GetTimestampValue().AsTime()
			}
		case "v":
			l.apply(readRecord(e))
		case "drop":
			if d := l.dbs[p["db"].GetStringValue()]; d != nil {
				l.dropLocked(p["db"].GetStringValue(), d)
			}
		case "snap":
			open[[2]string{p["project"].GetStringValue(), p["db"].GetStringValue()}] = true
		case "snapdone":
			delete(open, [2]string{p["project"].GetStringValue(), p["db"].GetStringValue()})
		}
	}
	var snaps [][2]string
	for k := range open {
		snaps = append(snaps, k)
	}
	return gen, snaps, good, nil
}

func uvarintLen(n uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], n)
}

func encode(e *pb.Entity) ([]byte, error) {
	b, err := proto.Marshal(e)
	if err != nil {
		return nil, err
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	return append(hdr[:n:n], b...), nil
}

// appendRecord writes e to the file's end.
func (s *store) appendRecord(e *pb.Entity) {
	b, err := encode(e)
	if err == nil {
		_, err = s.w.Write(b)
	}
	if err == nil {
		err = s.w.Flush()
	}
	if err != nil {
		s.logf("datastore versions: write the log: %v", err)
		return
	}
	s.size += int64(len(b))
}

// records is every record the log in memory is: its generation, its since
// times, every key's versions (the first with its base) and its open
// snapshots; l.mu is held.
func records(l *Log) []*pb.Entity {
	out := []*pb.Entity{genRecord(l), sinceRecord(l)}
	for db, d := range l.dbs {
		for id, h := range d.keys {
			for i, v := range h.versions {
				r := record{db: db, key: d.pk[id], at: v.at, ver: v.ver, entity: v.entity}
				if i == 0 {
					r.hasBase, r.base, r.baseVer = true, h.base, h.baseVer
				}
				out = append(out, versionRecord(r))
			}
		}
	}
	for _, sn := range l.snaps.open() {
		out = append(out, snapRecord("snap", sn[0], sn[1]))
	}
	return out
}

// rewrite replaces the file with the log in memory; l.mu is held.
func (s *store) rewrite(l *Log) error {
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	var size int64
	for _, e := range records(l) {
		b, err := encode(e)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := w.Write(b); err != nil {
			_ = f.Close()
			return err
		}
		size += int64(len(b))
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = f.Close()
		return fmt.Errorf("replace %s: %w", s.path, err)
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f, s.w, s.size = f, bufio.NewWriter(f), size
	return nil
}

// maybeCompact rewrites the file once it is twice the log's size; l.mu is
// held.
func (s *store) maybeCompact(l *Log) {
	if s.size <= 2*int64(l.bytes)+8<<20 {
		return
	}
	if err := s.rewrite(l); err != nil {
		s.logf("datastore versions: compact the log: %v", err)
	}
}

// Size is the log file's size (tests).
func (l *Log) FileSize() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return 0
	}
	return l.store.size
}

func (s *store) close() {
	if s.f != nil {
		_ = s.w.Flush()
		_ = s.f.Close()
	}
}
