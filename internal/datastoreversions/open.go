package datastoreversions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/metadata"
)

// markerKind is the kind of the generation marker, in the marker project.
const markerKind = "VersionLog"

// markerRecheck is how often the marker is read again: an emulator reset
// while the front runs drops it.
const markerRecheck = 5 * time.Second

type markerCheck struct {
	mu      sync.Mutex
	project string
	client  pb.DatastoreClient
	last    time.Time
}

func (m *markerCheck) key() *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: m.project},
		Path: []*pb.Key_PathElement{{Kind: markerKind, IdType: &pb.Key_PathElement_Name{Name: "generation"}}}}
}

func (m *markerCheck) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", "project_id="+m.project)
}

// read is the generation the emulator holds, "" for none.
func (m *markerCheck) read(ctx context.Context) (string, error) {
	resp, err := m.client.Lookup(m.ctx(ctx), &pb.LookupRequest{ProjectId: m.project, Keys: []*pb.Key{m.key()}})
	if err != nil {
		return "", err
	}
	for _, f := range resp.GetFound() {
		return f.GetEntity().GetProperties()["id"].GetStringValue(), nil
	}
	return "", nil
}

func (m *markerCheck) write(ctx context.Context, gen string) error {
	_, err := m.client.Commit(m.ctx(ctx), &pb.CommitRequest{ProjectId: m.project, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: &pb.Entity{Key: m.key(),
			Properties: map[string]*pb.Value{"id": str(gen)}}}}}})
	return err
}

func newGen() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Open loads the log from dir ("" keeps it in memory alone), once the
// emulator c answers, and keeps it: it prunes it every minute, and drops
// the snapshots reads leave. markerProject is the emulator project the
// front keeps its own records in. It returns when ctx ends; the log is
// ready once loaded.
func (l *Log) Open(ctx context.Context, c pb.DatastoreClient, markerProject, dir string) {
	l.marker = markerCheck{project: markerProject, client: c}
	l.snaps.client = c
	var leftover [][2]string
	if dir != "" {
		s, gen, snaps, err := openStore(dir, l)
		if err != nil {
			l.logf("datastore versions: open the log in %s: %v; keeping it in memory alone", dir, err)
			l.mu.Lock()
			l.dbs, l.bytes = map[string]*dbLog{}, 0
			l.mu.Unlock()
		} else {
			l.mu.Lock()
			l.store, l.gen = s, gen
			l.mu.Unlock()
			leftover = snaps
		}
	}
	// The emulator's marker says whether the log goes with its
	// entities.
	for {
		held, err := l.marker.read(ctx)
		if err == nil {
			l.mu.Lock()
			if held == "" || held != l.gen {
				if l.gen != "" {
					l.logf("datastore versions: the emulator's entities are not the ones the log was kept for; the versions begin again now")
				}
				if held == "" {
					held = newGen()
				}
				l.resetLocked(held)
			}
			l.mu.Unlock()
			if held != "" {
				if err := l.marker.write(ctx, held); err == nil {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	l.marker.mu.Lock()
	l.marker.last = time.Now()
	l.marker.mu.Unlock()
	for _, sn := range leftover {
		l.snaps.drop(ctx, sn[0], sn[1])
		l.noteSnapshot("snapdone", sn[0], sn[1])
	}
	l.Prune()
	l.markReady()
	l.logf("%s", l.String())
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			if l.store != nil {
				l.store.close()
			}
			l.mu.Unlock()
			return
		case <-t.C:
			l.Prune()
			l.recheck(ctx)
			l.sweepSnapshots(ctx, false)
		}
	}
}

// recheck reads the marker again, if it was last read more than
// markerRecheck ago, and starts the log again when the emulator no longer
// holds it.
func (l *Log) recheck(ctx context.Context) {
	l.marker.mu.Lock()
	defer l.marker.mu.Unlock()
	if l.marker.client == nil || time.Since(l.marker.last) < markerRecheck {
		return
	}
	held, err := l.marker.read(ctx)
	if err != nil {
		return
	}
	l.marker.last = time.Now()
	l.mu.Lock()
	gen := l.gen
	l.mu.Unlock()
	if held == gen {
		return
	}
	l.logf("datastore versions: the emulator's entities were reset; the versions begin again now")
	gen = newGen()
	if err := l.marker.write(ctx, gen); err != nil {
		return
	}
	l.mu.Lock()
	l.resetLocked(gen)
	l.mu.Unlock()
	l.sweepSnapshots(ctx, true)
}

func (l *Log) noteSnapshot(op, project, db string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		l.store.appendRecord(snapRecord(op, project, db))
	}
}
