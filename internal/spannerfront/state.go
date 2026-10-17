package spannerfront

// The front's state, kept in its state directory (#1514): the instance
// updates, operations, drop protection, IAM policies, backups and their
// restore information, backup schedules, database roles, custom instance
// configurations and partition updates would otherwise live only in the
// front's memory, so a restart of the front's container alone, the
// emulator still running and holding the instances and databases, would
// lose them. `cloudburrow up` keeps the directory on a size-limited
// emptyDir of the Spanner pod (internal/components), so it goes with the
// pod, as the emulator's data does. Each backup's rows are a copy file of
// their own (internal/frontstate).
//
// A change is written as the call that made it returns, and the schedule
// loop's backups within flushInterval.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

const flushInterval = time.Second

type savedChange struct {
	Instance []byte          `json:"instance"`
	Paths    map[string]bool `json:"paths,omitempty"`
	At       time.Time       `json:"at"`
}

type savedSchedule struct {
	Schedule []byte    `json:"schedule"`
	Last     time.Time `json:"last"`
}

type savedPartition struct {
	Partition []byte          `json:"partition"`
	Paths     map[string]bool `json:"paths,omitempty"`
}

// savedState is the records file's content; the messages are in their
// wire form.
type savedState struct {
	Changes    map[string]savedChange       `json:"changes,omitempty"`
	Ops        map[string][]byte            `json:"ops,omitempty"`
	OpOrder    []string                     `json:"opOrder,omitempty"`
	Protected  map[string]bool              `json:"protected,omitempty"`
	Policies   map[string]*iampolicy.Stored `json:"policies,omitempty"`
	Backups    map[string][]byte            `json:"backups,omitempty"`
	Restored   map[string][]byte            `json:"restored,omitempty"`
	Schedules  map[string]savedSchedule     `json:"schedules,omitempty"`
	RoleStmts  map[string][]string          `json:"roleStmts,omitempty"`
	EmuOpNames []string                     `json:"emuOpNames,omitempty"`
	Configs    map[string][]byte            `json:"configs,omitempty"`
	Partitions map[string]savedPartition    `json:"partitions,omitempty"`
	// SplitPoints are each database's split points (#1523), in their wire
	// form.
	SplitPoints map[string][][]byte `json:"splitPoints,omitempty"`
}

// snapshotHead is a backup's copy without its rows, the first part of its
// copy file; each table's rows follow, a structpb.ListValue of rows each.
type snapshotHead struct {
	Dialect int32    `json:"dialect"`
	DDL     []string `json:"ddl,omitempty"`
	Size    int64    `json:"size"`
	Tables  []struct {
		Name    string   `json:"name"`
		Columns []string `json:"columns"`
	} `json:"tables,omitempty"`
}

func encodeSnapshot(sn *snapshot) ([]byte, error) {
	head := snapshotHead{Dialect: int32(sn.dialect), DDL: sn.ddl, Size: sn.size}
	parts := [][]byte{nil}
	for _, t := range sn.tables {
		head.Tables = append(head.Tables, struct {
			Name    string   `json:"name"`
			Columns []string `json:"columns"`
		}{t.name, t.columns})
		lv := &structpb.ListValue{}
		for _, r := range t.rows {
			lv.Values = append(lv.Values, structpb.NewListValue(r))
		}
		b, err := proto.Marshal(lv)
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
	}
	h, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	parts[0] = h
	return frontstate.Pack(parts...), nil
}

func decodeSnapshot(b []byte) (*snapshot, error) {
	parts, err := frontstate.Unpack(b)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("an empty copy")
	}
	var head snapshotHead
	if err := json.Unmarshal(parts[0], &head); err != nil {
		return nil, err
	}
	if len(parts) != len(head.Tables)+1 {
		return nil, fmt.Errorf("%d tables named, %d held", len(head.Tables), len(parts)-1)
	}
	sn := &snapshot{dialect: databasepb.DatabaseDialect(head.Dialect), ddl: head.DDL, size: head.Size}
	for i, t := range head.Tables {
		lv := &structpb.ListValue{}
		if err := proto.Unmarshal(parts[i+1], lv); err != nil {
			return nil, err
		}
		tr := tableRows{name: t.Name, columns: t.Columns}
		for _, v := range lv.Values {
			tr.rows = append(tr.rows, v.GetListValue())
		}
		sn.tables = append(sn.tables, tr)
	}
	return sn, nil
}

// KeepState restores the front's state from dir, when it holds any, and
// from then on keeps it there, until ctx ends. It must be called before
// the front serves its first call. State that cannot be read is logged and
// replaced: the front starts without it.
func (s *Instances) KeepState(ctx context.Context, dir string, logf func(string, ...any)) error {
	st, err := frontstate.Open(dir)
	if err != nil {
		return err
	}
	s.logf = logf
	var saved savedState
	ok, err := st.Load(&saved)
	if err == nil && ok {
		err = s.restoreState(st, &saved)
	}
	if err != nil {
		logf("spanner front: the state in %s is unreadable, starting without it: %v", dir, err)
		if err := st.Clear(); err != nil {
			return err
		}
	} else if ok {
		logf("spanner front: restored %s", dir)
	}
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.persist()
				return
			case <-t.C:
				s.persist()
			}
		}
	}()
	return nil
}

func (s *Instances) restoreState(st *frontstate.Store, in *savedState) error {
	changes := make(map[string]*change, len(in.Changes))
	for name, c := range in.Changes {
		inst := &instancepb.Instance{}
		if err := proto.Unmarshal(c.Instance, inst); err != nil {
			return fmt.Errorf("the update of %s: %w", name, err)
		}
		changes[name] = &change{inst: inst, paths: c.Paths, at: c.At}
	}
	ops, err := frontstate.DecodeMap(in.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} })
	if err != nil {
		return err
	}
	pbs, err := frontstate.DecodeMap(in.Backups, func() *databasepb.Backup { return &databasepb.Backup{} })
	if err != nil {
		return err
	}
	backups := make(map[string]*backup, len(pbs))
	for name, pb := range pbs {
		raw, err := st.ReadCopy(name)
		if err != nil {
			return fmt.Errorf("the rows of the backup %s: %w", name, err)
		}
		sn, err := decodeSnapshot(raw)
		if err != nil {
			return fmt.Errorf("the rows of the backup %s: %w", name, err)
		}
		backups[name] = &backup{pb: pb, snap: sn}
		st.MarkRead(name, sn)
	}
	restored, err := frontstate.DecodeMap(in.Restored, func() *databasepb.RestoreInfo { return &databasepb.RestoreInfo{} })
	if err != nil {
		return err
	}
	schedules := make(map[string]*schedule, len(in.Schedules))
	for name, sc := range in.Schedules {
		pb := &databasepb.BackupSchedule{}
		if err := proto.Unmarshal(sc.Schedule, pb); err != nil {
			return fmt.Errorf("the schedule %s: %w", name, err)
		}
		c, err := checkSchedule("restore", pb)
		if err != nil {
			return fmt.Errorf("the schedule %s: %w", name, err)
		}
		schedules[name] = &schedule{pb: pb, cron: c, last: sc.Last}
	}
	configs, err := frontstate.DecodeMap(in.Configs, func() *instancepb.InstanceConfig { return &instancepb.InstanceConfig{} })
	if err != nil {
		return err
	}
	partitions := make(map[string]*partitionChange, len(in.Partitions))
	for name, p := range in.Partitions {
		part := &instancepb.InstancePartition{}
		if err := proto.Unmarshal(p.Partition, part); err != nil {
			return fmt.Errorf("the partition %s: %w", name, err)
		}
		partitions[name] = &partitionChange{part: part, paths: p.Paths}
	}
	splits := make(map[string][]*databasepb.SplitPoints, len(in.SplitPoints))
	for db, raw := range in.SplitPoints {
		for _, b := range raw {
			sp := &databasepb.SplitPoints{}
			if err := proto.Unmarshal(b, sp); err != nil {
				return fmt.Errorf("the split points of %s: %w", db, err)
			}
			splits[db] = append(splits[db], sp)
		}
	}
	policies := make(map[string]*iampolicy.Stored, len(in.Policies))
	for name, p := range in.Policies {
		if p != nil {
			policies[name] = p
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes, s.ops, s.opOrder = changes, ops, in.OpOrder
	s.protected = orEmpty(in.Protected)
	s.policies, s.backups, s.restored, s.schedules = policies, backups, restored, schedules
	s.roleStmts = map[string][]string{}
	for db, stmts := range in.RoleStmts {
		s.roleStmts[db] = stmts
	}
	s.emuOpNames, s.configs, s.partitions = in.EmuOpNames, configs, partitions
	s.splitPoints = splits
	return nil
}

func orEmpty(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}

// persist writes the state, logging a failure: the call that changed it
// has succeeded.
func (s *Instances) persist() {
	if err := s.save(); err != nil && s.logf != nil {
		s.logf("spanner front: %v", err)
	}
}

func (s *Instances) save() error {
	s.mu.Lock()
	st := s.state
	if st == nil {
		s.mu.Unlock()
		return nil
	}
	out, copies, err := s.snapshotState()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return st.Save(out, copies)
}

// snapshotState is the state to write. s.mu is held.
func (s *Instances) snapshotState() (*savedState, map[string]frontstate.Copy, error) {
	out := &savedState{OpOrder: append([]string(nil), s.opOrder...), EmuOpNames: append([]string(nil), s.emuOpNames...)}
	var err error
	if len(s.changes) > 0 {
		out.Changes = make(map[string]savedChange, len(s.changes))
	}
	for name, c := range s.changes {
		b, err := proto.Marshal(c.inst)
		if err != nil {
			return nil, nil, err
		}
		out.Changes[name] = savedChange{Instance: b, Paths: copyBools(c.paths), At: c.at}
	}
	if out.Ops, err = frontstate.EncodeMap(s.ops); err != nil {
		return nil, nil, err
	}
	out.Protected = copyBools(s.protected)
	if len(s.policies) > 0 {
		out.Policies = make(map[string]*iampolicy.Stored, len(s.policies))
		for name, p := range s.policies {
			c := *p
			out.Policies[name] = &c
		}
	}
	pbs := make(map[string]*databasepb.Backup, len(s.backups))
	copies := make(map[string]frontstate.Copy, len(s.backups))
	for name, b := range s.backups {
		pbs[name] = b.pb
		sn := b.snap
		if sn == nil {
			continue
		}
		copies[name] = frontstate.Copy{Data: sn, Encode: func() ([]byte, error) { return encodeSnapshot(sn) }}
	}
	if out.Backups, err = frontstate.EncodeMap(pbs); err != nil {
		return nil, nil, err
	}
	if out.Restored, err = frontstate.EncodeMap(s.restored); err != nil {
		return nil, nil, err
	}
	if len(s.schedules) > 0 {
		out.Schedules = make(map[string]savedSchedule, len(s.schedules))
	}
	for name, sc := range s.schedules {
		b, err := proto.Marshal(sc.pb)
		if err != nil {
			return nil, nil, err
		}
		out.Schedules[name] = savedSchedule{Schedule: b, Last: sc.last}
	}
	if len(s.splitPoints) > 0 {
		out.SplitPoints = make(map[string][][]byte, len(s.splitPoints))
		for db, sps := range s.splitPoints {
			for _, sp := range sps {
				b, err := proto.Marshal(sp)
				if err != nil {
					return nil, nil, err
				}
				out.SplitPoints[db] = append(out.SplitPoints[db], b)
			}
		}
	}
	if len(s.roleStmts) > 0 {
		out.RoleStmts = make(map[string][]string, len(s.roleStmts))
		for db, stmts := range s.roleStmts {
			out.RoleStmts[db] = append([]string(nil), stmts...)
		}
	}
	if out.Configs, err = frontstate.EncodeMap(s.configs); err != nil {
		return nil, nil, err
	}
	if len(s.partitions) > 0 {
		out.Partitions = make(map[string]savedPartition, len(s.partitions))
	}
	for name, p := range s.partitions {
		b, err := proto.Marshal(p.part)
		if err != nil {
			return nil, nil, err
		}
		out.Partitions[name] = savedPartition{Partition: b, Paths: copyBools(p.paths)}
	}
	return out, copies, nil
}

func copyBools(m map[string]bool) map[string]bool {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ResetState forgets everything the front keeps and clears its state
// directory (`cloudburrow reset`, #1514). The emulator's instances and
// databases are not touched.
func (s *Instances) ResetState() error {
	s.mu.Lock()
	s.changes = map[string]*change{}
	s.ops = map[string]*longrunningpb.Operation{}
	s.opOrder = nil
	s.protected = map[string]bool{}
	s.policies = map[string]*iampolicy.Stored{}
	s.backups = map[string]*backup{}
	s.restored = map[string]*databasepb.RestoreInfo{}
	s.schedules = map[string]*schedule{}
	s.roleStmts = map[string][]string{}
	s.emuOpNames = nil
	s.configs = map[string]*instancepb.InstanceConfig{}
	s.partitions = map[string]*partitionChange{}
	s.splitPoints = map[string][]*databasepb.SplitPoints{}
	st := s.state
	s.mu.Unlock()
	if st == nil {
		return nil
	}
	if err := st.Clear(); err != nil {
		return err
	}
	return s.save()
}

// persistAfter writes the state a gRPC call changed before it is answered.
func (x *Instances) persistAfter(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	resp, err := h(ctx, req)
	x.persist()
	return resp, err
}
