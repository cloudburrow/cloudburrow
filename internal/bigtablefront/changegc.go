package bigtablefront

// Garbage collection records (#1523). The emulator applies a column
// family's garbage collection rule on its own, every second or so, and
// tells no one, so the front watches the tables that have a change stream:
// it keeps the cells it last saw of each (cellModel), follows the writes it
// logs, and every gcPollInterval reads the table back, under the table's
// write lock so no write is in flight. A cell gone from the table that the
// family's rule collects (max_age, max_num_versions, an intersection or a
// union of them, as Google's garbage collection page defines them) is
// recorded as a GARBAGE_COLLECTION DataChange of its row, a DeleteFromColumn
// of the cell, with no source cluster, as the API documents for that type.
// A cell gone that the rule does not collect (written and removed out of
// the front's sight, by a restore say) is forgotten without a record.
//
// The table is read only while a family has a rule, and its cells are
// first read when the change stream is enabled, or when the front restarts
// (a cell collected while the front was down is not recorded).

import (
	"context"
	"sort"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// gcPollInterval is how often the tables with a change stream are read
// for collected cells; a variable so the tests can poll by hand.
var gcPollInterval = 2 * time.Second

// cellModel is a table's cells: row key, family, qualifier, timestamps.
type cellModel map[string]map[string]map[string]map[int64]bool

func (m cellModel) add(row, fam string, qual []byte, ts int64) {
	fams, ok := m[row]
	if !ok {
		fams = map[string]map[string]map[int64]bool{}
		m[row] = fams
	}
	cols, ok := fams[fam]
	if !ok {
		cols = map[string]map[int64]bool{}
		fams[fam] = cols
	}
	versions, ok := cols[string(qual)]
	if !ok {
		versions = map[int64]bool{}
		cols[string(qual)] = versions
	}
	versions[ts] = true
}

func (m cellModel) has(row, fam, qual string, ts int64) bool {
	return m[row][fam][qual][ts]
}

// apply follows the logged writes: a SetCell adds its cell, the deletes
// remove theirs. A nil model (not read yet) is left alone.
func (m cellModel) apply(rows []rowChange) {
	if m == nil {
		return
	}
	for _, r := range rows {
		row := string(r.row)
		for _, mu := range r.ms {
			switch x := mu.GetMutation().(type) {
			case *btpb.Mutation_SetCell_:
				m.add(row, x.SetCell.GetFamilyName(), x.SetCell.GetColumnQualifier(), x.SetCell.GetTimestampMicros())
			case *btpb.Mutation_AddToCell_:
				m.add(row, x.AddToCell.GetFamilyName(), x.AddToCell.GetColumnQualifier().GetRawValue(), x.AddToCell.GetTimestamp().GetRawTimestampMicros())
			case *btpb.Mutation_MergeToCell_:
				m.add(row, x.MergeToCell.GetFamilyName(), x.MergeToCell.GetColumnQualifier().GetRawValue(), x.MergeToCell.GetTimestamp().GetRawTimestampMicros())
			case *btpb.Mutation_DeleteFromColumn_:
				d := x.DeleteFromColumn
				versions := m[row][d.GetFamilyName()][string(d.GetColumnQualifier())]
				start, end := d.GetTimeRange().GetStartTimestampMicros(), d.GetTimeRange().GetEndTimestampMicros()
				for ts := range versions {
					if ts >= start && (end == 0 || ts < end) {
						delete(versions, ts)
					}
				}
			case *btpb.Mutation_DeleteFromFamily_:
				delete(m[row], x.DeleteFromFamily.GetFamilyName())
			case *btpb.Mutation_DeleteFromRow_:
				delete(m, row)
			}
		}
	}
}

// modelOf is the cells of rows.
func modelOf(rows []row) cellModel {
	m := cellModel{}
	for _, r := range rows {
		for _, c := range r.cells {
			m.add(string(r.key), c.family, c.qualifier, c.ts)
		}
	}
	return m
}

// collects reports whether rule collects a cell of timestamp ts, the
// rank-th newest of its column (from 0), at now.
func collects(rule *btapb.GcRule, ts int64, rank int, now time.Time) bool {
	switch r := rule.GetRule().(type) {
	case *btapb.GcRule_MaxNumVersions:
		return r.MaxNumVersions > 0 && rank >= int(r.MaxNumVersions)
	case *btapb.GcRule_MaxAge:
		return r.MaxAge.AsDuration() > 0 && ts < now.Add(-r.MaxAge.AsDuration()).UnixMicro()
	case *btapb.GcRule_Intersection_:
		rs := r.Intersection.GetRules()
		for _, sub := range rs {
			if !collects(sub, ts, rank, now) {
				return false
			}
		}
		return len(rs) > 0
	case *btapb.GcRule_Union_:
		for _, sub := range r.Union.GetRules() {
			if collects(sub, ts, rank, now) {
				return true
			}
		}
	}
	return false
}

// startGC starts the emulator's garbage collection when t has a family
// with a rule. The emulator (bttest) starts collecting only on its first
// ModifyColumnFamilies, so a table created with its rules would keep every
// cell; an empty ModifyColumnFamilies changes nothing and starts it.
func (f *Front) startGC(ctx context.Context, t *btapb.Table) {
	for _, fam := range t.GetColumnFamilies() {
		if fam.GetGcRule().GetRule() != nil {
			_, _ = btapb.NewBigtableTableAdminClient(f.upstream).ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: t.GetName()})
			return
		}
	}
}

// watchGarbage polls the tables with a change stream for collected cells
// until ctx ends.
func (f *Front) watchGarbage(ctx context.Context) {
	t := time.NewTicker(gcPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.changes.mu.Lock()
		var tables []string
		for name := range f.changes.tables {
			tables = append(tables, name)
		}
		f.changes.mu.Unlock()
		sort.Strings(tables)
		for _, name := range tables {
			f.collectGarbage(ctx, name)
		}
	}
}

// collectGarbage reads the table back and records the cells its families'
// rules collected since it was last read.
func (f *Front) collectGarbage(ctx context.Context, table string) {
	f.changes.mu.Lock()
	l, ok := f.changes.tables[table]
	f.changes.mu.Unlock()
	if !ok {
		return
	}
	l.write.Lock()
	defer l.write.Unlock()
	if l.gone {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: table, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		return
	}
	rules := map[string]*btapb.GcRule{}
	for name, fam := range t.GetColumnFamilies() {
		if fam.GetGcRule().GetRule() != nil {
			rules[name] = fam.GetGcRule()
		}
	}
	if len(rules) > 0 && !l.gcStarted {
		f.startGC(ctx, t) // the emulator may have restarted
		l.gcStarted = true
	}
	if l.cells != nil && len(rules) == 0 {
		return // nothing to collect, and the model follows the writes
	}
	rows, err := readAll(ctx, btpb.NewBigtableClient(f.upstream), &btpb.ReadRowsRequest{TableName: table})
	if err != nil {
		return
	}
	now := f.changes.now()
	cur := modelOf(rows)
	prev := l.cells
	l.cells = cur
	if prev == nil {
		return
	}
	var gc []rowChange
	for _, row := range sortedKeys(prev) {
		var ms []*btpb.Mutation
		for _, fam := range sortedKeys(prev[row]) {
			rule := rules[fam]
			if rule == nil {
				continue
			}
			for _, qual := range sortedKeys(prev[row][fam]) {
				var versions []int64
				for ts := range prev[row][fam][qual] {
					versions = append(versions, ts)
				}
				sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
				// Oldest first, as the cells were written.
				for rank := len(versions) - 1; rank >= 0; rank-- {
					ts := versions[rank]
					if cur.has(row, fam, qual, ts) || !collects(rule, ts, rank, now) {
						continue
					}
					// The cell's millisecond, the granularity a
					// table's timestamps have. Google documents no
					// time range for a collected cell's record. Docs
					// re-read 2026-11-17, still unanswered:
					// https://cloud.google.com/bigtable/docs/change-streams-overview
					// UNVERIFIED.
					ms = append(ms, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
						FamilyName: fam, ColumnQualifier: []byte(qual),
						TimeRange: &btpb.TimestampRange{StartTimestampMicros: ts, EndTimestampMicros: ts + 1000},
					}}})
				}
			}
		}
		if len(ms) > 0 {
			gc = append(gc, rowChange{row: []byte(row), ms: ms})
		}
	}
	if len(gc) == 0 {
		return
	}
	f.changes.mu.Lock()
	if !l.gone {
		for _, r := range gc {
			f.changes.appendLocked(l, btpb.ReadChangeStreamResponse_DataChange_GARBAGE_COLLECTION, r.row, r.ms, "")
		}
	}
	f.changes.mu.Unlock()
	f.flush()
}
