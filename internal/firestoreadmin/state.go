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
	"slices"
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
	// PITRSince is when each database's point-in-time recovery was last
	// enabled (pitr.go, #1942), by database name.
	PITRSince map[string]time.Time `json:"pitrSince,omitempty"`
	// BackupKeys are the backups' customer-managed encryption, by backup
	// name (#1881).
	BackupKeys map[string][]byte `json:"backupKeys,omitempty"`
	// EntityBackups are the backups of Datastore mode databases, whose
	// copies are Datastore entities (#1884).
	EntityBackups []string `json:"entityBackups,omitempty"`
	// Seen is every database a documents call has named (seen.go, #1550).
	Seen []string `json:"seen,omitempty"`
	// UserCreds are the user credentials, each with its password's
	// digest, never the password (#1500).
	UserCreds map[string]savedCreds `json:"userCreds,omitempty"`
	// MongoIndexNames are the MongoDB names of the indexes MongoDB drivers
	// created, by index name (#1703).
	MongoIndexNames map[string]string `json:"mongoIndexNames,omitempty"`
}

type savedCreds struct {
	UserCreds []byte `json:"userCreds"`
	Digest    []byte `json:"digest"`
	// Scram is the password's SCRAM-SHA-256 verifier (#1703).
	Scram *scramKeys `json:"scram,omitempty"`
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
	keys, err := frontstate.DecodeMap(s.BackupKeys, func() *adminpb.Database_CmekConfig { return &adminpb.Database_CmekConfig{} })
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
		b := &backup{meta: meta, cmek: keys[name]}
		if slices.Contains(s.EntityBackups, name) {
			b.datastore, b.entities = true, parts
			backups[name] = b
			st.MarkRead(name, b)
			continue
		}
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
	userCreds := make(map[string]*creds, len(s.UserCreds))
	for name, sc := range s.UserCreds {
		uc := &adminpb.UserCreds{}
		if err := proto.Unmarshal(sc.UserCreds, uc); err != nil {
			return fmt.Errorf("the user creds %s: %w", name, err)
		}
		userCreds[name] = &creds{UserCreds: uc, Digest: sc.Digest, Scram: sc.Scram}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.userCreds = userCreds
	a.mongoIndexNames = map[string]string{}
	for k, v := range s.MongoIndexNames {
		a.mongoIndexNames[k] = v
	}
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
	a.pitrSince = map[string]time.Time{}
	for n, t := range s.PITRSince {
		a.pitrSince[n] = t
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
	backupKeys := map[string]*adminpb.Database_CmekConfig{}
	for name, b := range a.backups {
		metas[name] = b.meta
		if b.cmek != nil {
			backupKeys[name] = b.cmek
		}
		if b.datastore {
			s.EntityBackups = append(s.EntityBackups, name)
			entities := b.entities
			copies[name] = frontstate.Copy{Data: b, Encode: func() ([]byte, error) { return frontstate.Pack(entities...), nil }}
			continue
		}
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
	slices.Sort(s.EntityBackups)
	m, e = frontstate.EncodeMap(metas)
	enc(&s.Backups, m, e)
	if len(backupKeys) > 0 {
		m, e = frontstate.EncodeMap(backupKeys)
		enc(&s.BackupKeys, m, e)
	}
	for name, c := range a.userCreds {
		b, e := proto.Marshal(c.UserCreds)
		if e != nil && err == nil {
			err = e
		}
		if s.UserCreds == nil {
			s.UserCreds = map[string]savedCreds{}
		}
		s.UserCreds[name] = savedCreds{UserCreds: b, Digest: c.Digest, Scram: c.Scram}
	}
	if len(a.mongoIndexNames) > 0 {
		s.MongoIndexNames = make(map[string]string, len(a.mongoIndexNames))
		for k, v := range a.mongoIndexNames {
			s.MongoIndexNames[k] = v
		}
	}
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
	if len(a.pitrSince) > 0 {
		s.PITRSince = make(map[string]time.Time, len(a.pitrSince))
		for n, t := range a.pitrSince {
			s.PITRSince[n] = t
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
// operation, backup and user's credentials, and clears the state directory
// (`cloudburrow reset`, #1514). The emulator's documents are not touched,
// so the databases that hold them stay seen (#1636): the documents' export
// (documents.go) and a project's purge (purge.go) still reach them. A
// database the reset forgets would otherwise have its documents left out
// of every later export, and lost at the next `stop` and `up`.
func (a *Admin) ResetState() error {
	a.mu.Lock()
	for n := range a.databases {
		if db := databaseOf(n); db == n {
			a.seen[db] = true
		}
	}
	a.databases = map[string]*adminpb.Database{}
	a.seeded = map[string]bool{}
	a.indexes = map[string]*adminpb.Index{}
	a.fields = map[string]*adminpb.Field{}
	a.schedules = map[string]*adminpb.BackupSchedule{}
	a.ops = map[string]*longrunningpb.Operation{}
	a.opOrder = nil
	a.backups = map[string]*backup{}
	a.lastBackup = map[string]time.Time{}
	a.pitrSince = map[string]time.Time{}
	a.userCreds = map[string]*creds{}
	a.mongoIndexNames = map[string]string{}
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
