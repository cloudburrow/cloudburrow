package bigtablefront

// The front's state, kept in its state directory (#1514): the instances
// with their clusters, app profiles and IAM policies, the operations, the
// memory layers, the backups and snapshots with their rows, the deleted
// tables kept for UndeleteTable, the authorized, logical and materialized
// views, the schema bundles and the table IAM policies would otherwise
// live only in the front's memory, so a restart of the front's container
// alone, the emulator still running and holding the tables, would lose
// them. `cloudburrow up` keeps the directory on a size-limited emptyDir of
// the Bigtable pod (internal/components), so it goes with the pod, as the
// emulator's tables do. Each copy's rows are a copy file of their own
// (internal/frontstate).
//
// A change is written as the call that made it returns, and anything else
// within flushInterval.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

const flushInterval = time.Second

type savedInstance struct {
	Instance []byte            `json:"instance"`
	Clusters map[string][]byte `json:"clusters,omitempty"`
	Profiles map[string][]byte `json:"profiles,omitempty"`
	Policy   *iampolicy.Stored `json:"policy,omitempty"`
}

type savedCopy struct {
	Meta []byte `json:"meta"`
}

type savedDeleted struct {
	At time.Time `json:"at"`
}

type savedSQLView struct {
	Query     string `json:"query"`
	Etag      string `json:"etag"`
	Protected bool   `json:"protected,omitempty"`
}

// savedState is the records file's content; the messages are in their
// wire form.
type savedState struct {
	Instances    map[string]savedInstance     `json:"instances,omitempty"`
	Ops          map[string][]byte            `json:"ops,omitempty"`
	OpsQ         []string                     `json:"opsQ,omitempty"`
	Layers       map[string][]byte            `json:"layers,omitempty"`
	Backups      map[string]savedCopy         `json:"backups,omitempty"`
	Snapshots    map[string]savedCopy         `json:"snapshots,omitempty"`
	Deleted      map[string]savedDeleted      `json:"deleted,omitempty"`
	AuthViews    map[string][]byte            `json:"authViews,omitempty"`
	SQLViews     map[string]savedSQLView      `json:"sqlViews,omitempty"`
	Bundles      map[string][]byte            `json:"bundles,omitempty"`
	TablePolicys map[string]*iampolicy.Stored `json:"tablePolicies,omitempty"`
}

// persister is the front's state directory.
type persister struct {
	mu    sync.Mutex
	state *frontstate.Store
}

func encodeTableCopy(c *tableCopy) ([]byte, error) {
	fam, err := proto.Marshal(&btapb.Table{ColumnFamilies: c.families})
	if err != nil {
		return nil, err
	}
	rows, err := proto.Marshal(&btpb.MutateRowsRequest{Entries: c.rows})
	if err != nil {
		return nil, err
	}
	size, err := json.Marshal(c.size)
	if err != nil {
		return nil, err
	}
	return frontstate.Pack(fam, rows, size), nil
}

func decodeTableCopy(b []byte) (*tableCopy, error) {
	parts, err := frontstate.Unpack(b)
	if err != nil {
		return nil, err
	}
	if len(parts) != 3 {
		return nil, fmt.Errorf("a copy of %d parts, want 3", len(parts))
	}
	t, rows := &btapb.Table{}, &btpb.MutateRowsRequest{}
	if err := proto.Unmarshal(parts[0], t); err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(parts[1], rows); err != nil {
		return nil, err
	}
	c := &tableCopy{families: t.ColumnFamilies, rows: rows.Entries}
	if c.families == nil {
		c.families = map[string]*btapb.ColumnFamily{}
	}
	if err := json.Unmarshal(parts[2], &c.size); err != nil {
		return nil, err
	}
	return c, nil
}

// Copy keys: a backup, a snapshot and a deleted table may share a name's
// last segment, never a key.
func backupKey(name string) string   { return "backup:" + name }
func snapshotKey(name string) string { return "snapshot:" + name }
func deletedKey(name string) string  { return "deleted:" + name }

// KeepState restores the front's state from dir, when it holds any, and
// from then on keeps it there, until ctx ends. It must be called before
// the front serves its first call. State that cannot be read is logged and
// replaced: the front starts without it.
func (f *Front) KeepState(ctx context.Context, dir string) error {
	st, err := frontstate.Open(dir)
	if err != nil {
		return err
	}
	var s savedState
	ok, err := st.Load(&s)
	if err == nil && ok {
		err = f.restoreState(st, &s)
	}
	if err != nil {
		f.logf("bigtable front: the state in %s is unreadable, starting without it: %v", dir, err)
		if err := st.Clear(); err != nil {
			return err
		}
	} else if ok {
		f.logf("bigtable front: restored %s", dir)
	}
	f.persist.mu.Lock()
	f.persist.state = st
	f.persist.mu.Unlock()
	if err := f.save(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				f.flush()
				return
			case <-t.C:
				f.flush()
			}
		}
	}()
	return nil
}

func (f *Front) restoreState(st *frontstate.Store, s *savedState) error {
	byNm := make(map[string]*instance, len(s.Instances))
	for name, si := range s.Instances {
		in := &instance{pb: &btapb.Instance{}, policy: si.Policy}
		if err := proto.Unmarshal(si.Instance, in.pb); err != nil {
			return fmt.Errorf("the instance %s: %w", name, err)
		}
		var err error
		if in.clusters, err = frontstate.DecodeMap(si.Clusters, func() *btapb.Cluster { return &btapb.Cluster{} }); err != nil {
			return err
		}
		if in.profiles, err = frontstate.DecodeMap(si.Profiles, func() *btapb.AppProfile { return &btapb.AppProfile{} }); err != nil {
			return err
		}
		byNm[name] = in
	}
	ops, err := frontstate.DecodeMap(s.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} })
	if err != nil {
		return err
	}
	layers, err := frontstate.DecodeMap(s.Layers, func() *btapb.MemoryLayer { return &btapb.MemoryLayer{} })
	if err != nil {
		return err
	}
	readCopy := func(key string) (*tableCopy, error) {
		raw, err := st.ReadCopy(key)
		if err != nil {
			return nil, fmt.Errorf("the rows of %s: %w", key, err)
		}
		c, err := decodeTableCopy(raw)
		if err != nil {
			return nil, fmt.Errorf("the rows of %s: %w", key, err)
		}
		st.MarkRead(key, c)
		return c, nil
	}
	backups := make(map[string]*backupEntry, len(s.Backups))
	for name, b := range s.Backups {
		e := &backupEntry{pb: &btapb.Backup{}}
		if err := proto.Unmarshal(b.Meta, e.pb); err != nil {
			return fmt.Errorf("the backup %s: %w", name, err)
		}
		if e.data, err = readCopy(backupKey(name)); err != nil {
			return err
		}
		backups[name] = e
	}
	snapshots := make(map[string]*snapshotEntry, len(s.Snapshots))
	for name, b := range s.Snapshots {
		e := &snapshotEntry{pb: &btapb.Snapshot{}}
		if err := proto.Unmarshal(b.Meta, e.pb); err != nil {
			return fmt.Errorf("the snapshot %s: %w", name, err)
		}
		if e.data, err = readCopy(snapshotKey(name)); err != nil {
			return err
		}
		snapshots[name] = e
	}
	deleted := make(map[string]*deletedTable, len(s.Deleted))
	for name, d := range s.Deleted {
		c, err := readCopy(deletedKey(name))
		if err != nil {
			return err
		}
		deleted[name] = &deletedTable{data: c, at: d.At}
	}
	views, err := frontstate.DecodeMap(s.AuthViews, func() *btapb.AuthorizedView { return &btapb.AuthorizedView{} })
	if err != nil {
		return err
	}
	bundles, err := frontstate.DecodeMap(s.Bundles, func() *btapb.SchemaBundle { return &btapb.SchemaBundle{} })
	if err != nil {
		return err
	}
	sqlv := make(map[string]*sqlView, len(s.SQLViews))
	for name, v := range s.SQLViews {
		sqlv[name] = &sqlView{name: name, query: v.Query, etag: v.Etag, protected: v.Protected}
	}
	policies := make(map[string]*iampolicy.Stored, len(s.TablePolicys))
	for name, p := range s.TablePolicys {
		if p != nil {
			policies[name] = p
		}
	}
	f.instances.mu.Lock()
	f.instances.byNm, f.instances.ops, f.instances.opsQ, f.instances.layers = byNm, ops, s.OpsQ, layers
	f.instances.mu.Unlock()
	f.copies.mu.Lock()
	f.copies.backups, f.copies.snapshots, f.copies.deleted = backups, snapshots, deleted
	f.copies.mu.Unlock()
	f.views.mu.Lock()
	f.views.byName = views
	f.views.mu.Unlock()
	f.sqlViews.mu.Lock()
	f.sqlViews.byName = sqlv
	f.sqlViews.mu.Unlock()
	f.bundles.mu.Lock()
	f.bundles.byName = bundles
	f.bundles.mu.Unlock()
	f.tableIAM.mu.Lock()
	f.tableIAM.policies = policies
	f.tableIAM.mu.Unlock()
	return nil
}

// flush writes the state, logging a failure: the call that changed it has
// succeeded.
func (f *Front) flush() {
	if err := f.save(); err != nil {
		f.logf("bigtable front: %v", err)
	}
}

func (f *Front) save() error {
	f.persist.mu.Lock()
	defer f.persist.mu.Unlock()
	st := f.persist.state
	if st == nil {
		return nil
	}
	s, copies, err := f.snapshotState()
	if err != nil {
		return err
	}
	return st.Save(s, copies)
}

// snapshotState is the state to write, each part read under its own lock.
func (f *Front) snapshotState() (*savedState, map[string]frontstate.Copy, error) {
	s := &savedState{}
	copies := map[string]frontstate.Copy{}
	var err error
	in := f.instances
	in.mu.Lock()
	if len(in.byNm) > 0 {
		s.Instances = make(map[string]savedInstance, len(in.byNm))
	}
	for name, i := range in.byNm {
		si := savedInstance{}
		if si.Instance, err = proto.Marshal(i.pb); err == nil {
			if si.Clusters, err = frontstate.EncodeMap(i.clusters); err == nil {
				si.Profiles, err = frontstate.EncodeMap(i.profiles)
			}
		}
		if err != nil {
			in.mu.Unlock()
			return nil, nil, err
		}
		if i.policy != nil {
			p := *i.policy
			si.Policy = &p
		}
		s.Instances[name] = si
	}
	if s.Ops, err = frontstate.EncodeMap(in.ops); err == nil {
		s.Layers, err = frontstate.EncodeMap(in.layers)
	}
	s.OpsQ = append([]string(nil), in.opsQ...)
	in.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	addCopy := func(key string, c *tableCopy) {
		if c != nil {
			copies[key] = frontstate.Copy{Data: c, Encode: func() ([]byte, error) { return encodeTableCopy(c) }}
		}
	}
	c := f.copies
	c.mu.Lock()
	if len(c.backups) > 0 {
		s.Backups = make(map[string]savedCopy, len(c.backups))
	}
	for name, b := range c.backups {
		m, e := proto.Marshal(b.pb)
		if e != nil {
			err = e
		}
		s.Backups[name] = savedCopy{Meta: m}
		addCopy(backupKey(name), b.data)
	}
	if len(c.snapshots) > 0 {
		s.Snapshots = make(map[string]savedCopy, len(c.snapshots))
	}
	for name, sn := range c.snapshots {
		m, e := proto.Marshal(sn.pb)
		if e != nil {
			err = e
		}
		s.Snapshots[name] = savedCopy{Meta: m}
		addCopy(snapshotKey(name), sn.data)
	}
	if len(c.deleted) > 0 {
		s.Deleted = make(map[string]savedDeleted, len(c.deleted))
	}
	for name, d := range c.deleted {
		s.Deleted[name] = savedDeleted{At: d.at}
		addCopy(deletedKey(name), d.data)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	f.views.mu.Lock()
	s.AuthViews, err = frontstate.EncodeMap(f.views.byName)
	f.views.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	f.bundles.mu.Lock()
	s.Bundles, err = frontstate.EncodeMap(f.bundles.byName)
	f.bundles.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	f.sqlViews.mu.Lock()
	if len(f.sqlViews.byName) > 0 {
		s.SQLViews = make(map[string]savedSQLView, len(f.sqlViews.byName))
	}
	for name, v := range f.sqlViews.byName {
		s.SQLViews[name] = savedSQLView{Query: v.query, Etag: v.etag, Protected: v.protected}
	}
	f.sqlViews.mu.Unlock()
	f.tableIAM.mu.Lock()
	if len(f.tableIAM.policies) > 0 {
		s.TablePolicys = make(map[string]*iampolicy.Stored, len(f.tableIAM.policies))
	}
	for name, p := range f.tableIAM.policies {
		c := *p
		s.TablePolicys[name] = &c
	}
	f.tableIAM.mu.Unlock()
	return s, copies, nil
}

// ResetState drops every table of every instance the front keeps from the
// emulator, forgets everything the front keeps, and clears its state
// directory (`cloudburrow reset`, #1514): the tables belong to the
// instances, which only the front holds.
func (f *Front) ResetState(ctx context.Context) error {
	f.instances.mu.Lock()
	var names []string
	for name := range f.instances.byNm {
		names = append(names, name)
	}
	f.instances.byNm = map[string]*instance{}
	f.instances.ops = map[string]*longrunningpb.Operation{}
	f.instances.opsQ = nil
	f.instances.layers = map[string]*btapb.MemoryLayer{}
	f.instances.mu.Unlock()
	var dropErr error
	for _, name := range names {
		if err := f.dropTables(ctx, name); err != nil && dropErr == nil {
			dropErr = fmt.Errorf("drop the tables of %s: %w", name, err)
		}
	}
	f.copies.mu.Lock()
	f.copies.backups, f.copies.snapshots, f.copies.deleted = map[string]*backupEntry{}, map[string]*snapshotEntry{}, nil
	f.copies.mu.Unlock()
	f.views.mu.Lock()
	f.views.byName = map[string]*btapb.AuthorizedView{}
	f.views.mu.Unlock()
	f.sqlViews.mu.Lock()
	f.sqlViews.byName = map[string]*sqlView{}
	f.sqlViews.mu.Unlock()
	f.bundles.mu.Lock()
	f.bundles.byName = map[string]*btapb.SchemaBundle{}
	f.bundles.mu.Unlock()
	f.tableIAM.mu.Lock()
	f.tableIAM.policies = map[string]*iampolicy.Stored{}
	f.tableIAM.mu.Unlock()
	f.persist.mu.Lock()
	st := f.persist.state
	f.persist.mu.Unlock()
	if st != nil {
		if err := st.Clear(); err != nil {
			return err
		}
	}
	if err := f.save(); err != nil {
		return err
	}
	return dropErr
}

// serveReset answers frontstate.ResetPath.
func (f *Front) serveReset(w http.ResponseWriter, r *http.Request) {
	frontstate.ServeReset(w, r, func() error { return f.ResetState(r.Context()) })
}
