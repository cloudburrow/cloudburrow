package firestoreadmin

// The Admin API's state, kept in the front's state directory (#1514):
// the databases, indexes, field overrides, backup schedules, operations and
// backups would otherwise live only in the front's memory, so a restart of
// the front's container alone, the emulator still running and holding the
// documents, would lose them. `cloudburrow up` keeps the directory on a
// size-limited emptyDir of the Firestore pod (internal/components), so it
// goes with the pod, as the emulator's documents do. Each backup's
// documents are a copy file of their own (internal/frontstate).
//
// A change is written as the call that made it returns, and the schedule
// loop's backups within flushInterval.

import (
	"context"
	"fmt"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

const flushInterval = time.Second

// savedState is the records file's content; the messages are in their
// wire form.
type savedState struct {
	Databases  map[string][]byte    `json:"databases,omitempty"`
	Seeded     map[string]bool      `json:"seeded,omitempty"`
	Indexes    map[string][]byte    `json:"indexes,omitempty"`
	Fields     map[string][]byte    `json:"fields,omitempty"`
	Schedules  map[string][]byte    `json:"schedules,omitempty"`
	Ops        map[string][]byte    `json:"ops,omitempty"`
	OpOrder    []string             `json:"opOrder,omitempty"`
	Backups    map[string][]byte    `json:"backups,omitempty"`
	LastBackup map[string]time.Time `json:"lastBackup,omitempty"`
	// Seen is every database a documents call has named (seen.go, #1550).
	Seen []string `json:"seen,omitempty"`
}

// KeepState restores the API's state from dir, when it holds any, and
// from then on keeps it there, until ctx ends. It must be called before
// the API serves its first call. State that cannot be read is logged and
// replaced: the API starts without it.
func (a *Admin) KeepState(ctx context.Context, dir string, logf func(string, ...any)) error {
	st, err := frontstate.Open(dir)
	if err != nil {
		return err
	}
	a.logf = logf
	var s savedState
	ok, err := st.Load(&s)
	if err == nil && ok {
		err = a.restore(st, &s)
	}
	if err != nil {
		logf("firestore admin: the state in %s is unreadable, starting without it: %v", dir, err)
		if err := st.Clear(); err != nil {
			return err
		}
	} else if ok {
		logf("firestore admin: restored %s", dir)
	}
	a.mu.Lock()
	a.state = st
	a.mu.Unlock()
	if err := a.save(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				a.persist()
				return
			case <-t.C:
				a.persist()
			}
		}
	}()
	return nil
}

func (a *Admin) restore(st *frontstate.Store, s *savedState) error {
	dbs, err := frontstate.DecodeMap(s.Databases, func() *adminpb.Database { return &adminpb.Database{} })
	if err != nil {
		return err
	}
	idx, err := frontstate.DecodeMap(s.Indexes, func() *adminpb.Index { return &adminpb.Index{} })
	if err != nil {
		return err
	}
	fields, err := frontstate.DecodeMap(s.Fields, func() *adminpb.Field { return &adminpb.Field{} })
	if err != nil {
		return err
	}
	scheds, err := frontstate.DecodeMap(s.Schedules, func() *adminpb.BackupSchedule { return &adminpb.BackupSchedule{} })
	if err != nil {
		return err
	}
	ops, err := frontstate.DecodeMap(s.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} })
	if err != nil {
		return err
	}
	metas, err := frontstate.DecodeMap(s.Backups, func() *adminpb.Backup { return &adminpb.Backup{} })
	if err != nil {
		return err
	}
	backups := make(map[string]*backup, len(metas))
	for name, meta := range metas {
		raw, err := st.ReadCopy(name)
		if err != nil {
			return fmt.Errorf("the documents of the backup %s: %w", name, err)
		}
		parts, err := frontstate.Unpack(raw)
		if err != nil {
			return fmt.Errorf("the documents of the backup %s: %w", name, err)
		}
		b := &backup{meta: meta}
		for _, p := range parts {
			d := &firestorepb.Document{}
			if err := proto.Unmarshal(p, d); err != nil {
				return fmt.Errorf("a document of the backup %s: %w", name, err)
			}
			b.docs = append(b.docs, d)
		}
		backups[name] = b
		st.MarkRead(name, b)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.databases, a.indexes, a.fields, a.schedules, a.ops, a.backups = dbs, idx, fields, scheds, ops, backups
	a.opOrder = s.OpOrder
	a.seeded = map[string]bool{}
	for p, v := range s.Seeded {
		a.seeded[p] = v
	}
	a.lastBackup = map[string]time.Time{}
	for n, t := range s.LastBackup {
		a.lastBackup[n] = t
	}
	a.seen = map[string]bool{}
	for _, n := range s.Seen {
		if db := databaseOf(n); db != "" {
			a.seen[db] = true
		}
	}
	return nil
}

// persist writes the state, logging a failure: the call that changed it
// has succeeded.
func (a *Admin) persist() {
	if err := a.save(); err != nil && a.logf != nil {
		a.logf("firestore admin: %v", err)
	}
}

func (a *Admin) save() error {
	a.mu.Lock()
	st := a.state
	if st == nil {
		a.mu.Unlock()
		return nil
	}
	var s savedState
	var err error
	enc := func(dst *map[string][]byte, m map[string][]byte, e error) {
		if err == nil {
			*dst, err = m, e
		}
	}
	m, e := frontstate.EncodeMap(a.databases)
	enc(&s.Databases, m, e)
	m, e = frontstate.EncodeMap(a.indexes)
	enc(&s.Indexes, m, e)
	m, e = frontstate.EncodeMap(a.fields)
	enc(&s.Fields, m, e)
	m, e = frontstate.EncodeMap(a.schedules)
	enc(&s.Schedules, m, e)
	m, e = frontstate.EncodeMap(a.ops)
	enc(&s.Ops, m, e)
	metas := make(map[string]*adminpb.Backup, len(a.backups))
	copies := make(map[string]frontstate.Copy, len(a.backups))
	for name, b := range a.backups {
		metas[name] = b.meta
		docs := b.docs
		copies[name] = frontstate.Copy{Data: b, Encode: func() ([]byte, error) {
			parts := make([][]byte, 0, len(docs))
			for _, d := range docs {
				p, err := proto.Marshal(d)
				if err != nil {
					return nil, err
				}
				parts = append(parts, p)
			}
			return frontstate.Pack(parts...), nil
		}}
	}
	m, e = frontstate.EncodeMap(metas)
	enc(&s.Backups, m, e)
	s.OpOrder = append([]string(nil), a.opOrder...)
	if len(a.seeded) > 0 {
		s.Seeded = make(map[string]bool, len(a.seeded))
		for p, v := range a.seeded {
			s.Seeded[p] = v
		}
	}
	if len(a.lastBackup) > 0 {
		s.LastBackup = make(map[string]time.Time, len(a.lastBackup))
		for n, t := range a.lastBackup {
			s.LastBackup[n] = t
		}
	}
	s.Seen = a.seenList("")
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return st.Save(&s, copies)
}

// ResetState forgets every database, index, field override, schedule,
// operation and backup, and clears the state directory
// (`cloudburrow reset`, #1514). The emulator's documents are not touched.
func (a *Admin) ResetState() error {
	a.mu.Lock()
	a.databases = map[string]*adminpb.Database{}
	a.seeded = map[string]bool{}
	a.indexes = map[string]*adminpb.Index{}
	a.fields = map[string]*adminpb.Field{}
	a.schedules = map[string]*adminpb.BackupSchedule{}
	a.ops = map[string]*longrunningpb.Operation{}
	a.opOrder = nil
	a.backups = map[string]*backup{}
	a.lastBackup = map[string]time.Time{}
	a.seen = map[string]bool{}
	st := a.state
	a.mu.Unlock()
	if st == nil {
		return nil
	}
	if err := st.Clear(); err != nil {
		return err
	}
	return a.save()
}

// persistAfter writes the state a gRPC call changed before it is answered.
func (x *Admin) persistAfter(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	resp, err := h(ctx, req)
	x.persist()
	return resp, err
}
