package spannerfront

import (
	"context"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AddSplitPoints (#1523). Google documents split points as a hint: a
// database is pre-split at them, for a load expected to come, until they
// expire (by default in 10 days, at most 30), and load-based splitting
// carries on as before, so a split changes how a database's data is
// placed, never what any call reads or writes. The emulator is one server
// that never splits, so the front checks the request as Google does (the
// database, each table and index in its schema, an index on the table
// named with it, a key for each split, an expiry no more than 30 days
// ahead) and keeps the split points that have not expired with the
// database, as a stored no-op.

const (
	splitDefaultExpiry = 10 * 24 * time.Hour
	splitMaxExpiry     = 30 * 24 * time.Hour
)

var (
	ddlTableRe = regexp.MustCompile("(?is)^\\s*CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?[`\"]?([\\w.]+)[`\"]?")
	ddlIndexRe = regexp.MustCompile("(?is)^\\s*CREATE\\s+(?:UNIQUE\\s+)?(?:NULL_FILTERED\\s+)?(?:SEARCH\\s+|VECTOR\\s+)?INDEX\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?[`\"]?([\\w.]+)[`\"]?\\s+ON\\s+[`\"]?([\\w.]+)")
)

func (d databases) AddSplitPoints(ctx context.Context, req *databasepb.AddSplitPointsRequest) (*databasepb.AddSplitPointsResponse, error) {
	if !databaseNameRe.MatchString(req.GetDatabase()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid AddSplitPoints request: database %q is not projects/{project}/instances/{instance}/databases/{database}.", req.GetDatabase())
	}
	if len(req.GetSplitPoints()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Invalid AddSplitPoints request: split_points is required.")
	}
	ddl, err := d.s.emuDB.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: req.GetDatabase()})
	if err != nil {
		return nil, err
	}
	tables, indexes := map[string]bool{}, map[string]string{}
	for _, st := range ddl.GetStatements() {
		if m := ddlTableRe.FindStringSubmatch(st); m != nil {
			tables[strings.ToLower(m[1])] = true
		} else if m := ddlIndexRe.FindStringSubmatch(st); m != nil {
			indexes[strings.ToLower(m[1])] = strings.ToLower(m[2])
		}
	}
	now := d.s.now()
	var kept []*databasepb.SplitPoints
	for _, sp := range req.GetSplitPoints() {
		table, index := sp.GetTable(), sp.GetIndex()
		switch {
		case table == "" && index == "":
			return nil, status.Error(codes.InvalidArgument, "Invalid AddSplitPoints request: each split point names a table or an index.")
		case index != "":
			base, ok := indexes[strings.ToLower(index)]
			if !ok {
				return nil, status.Errorf(codes.NotFound, "Index not found: %s", index)
			}
			if table != "" && !strings.EqualFold(table, base) {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid AddSplitPoints request: the index %s is on the table %s, not %s.", index, base, table)
			}
		case !tables[strings.ToLower(table)]:
			return nil, status.Errorf(codes.NotFound, "Table not found: %s", table)
		}
		if len(sp.GetKeys()) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid AddSplitPoints request: a split point of %s%s has no keys.", table, index)
		}
		for _, k := range sp.GetKeys() {
			if len(k.GetKeyParts().GetValues()) == 0 {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid AddSplitPoints request: a split key of %s%s has no key parts.", table, index)
			}
		}
		expiry := now.Add(splitDefaultExpiry)
		if e := sp.GetExpireTime(); e != nil {
			expiry = e.AsTime()
		}
		if expiry.After(now.Add(splitMaxExpiry)) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid AddSplitPoints request: expire_time %s is more than 30 days ahead.", expiry.Format(time.RFC3339))
		}
		if !expiry.After(now) {
			// A time in the past expires the split points at once.
			continue
		}
		c := proto.Clone(sp).(*databasepb.SplitPoints)
		c.ExpireTime = timestamppb.New(expiry)
		kept = append(kept, c)
	}
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	var live []*databasepb.SplitPoints
	for _, sp := range d.s.splitPoints[req.GetDatabase()] {
		if sp.GetExpireTime().AsTime().After(now) {
			live = append(live, sp)
		}
	}
	if live = append(live, kept...); len(live) > 0 {
		d.s.splitPoints[req.GetDatabase()] = live
	} else {
		delete(d.s.splitPoints, req.GetDatabase())
	}
	return &databasepb.AddSplitPointsResponse{}, nil
}

// dropSplitPoints forgets the split points of database and of any under
// it. s.mu is held.
func (s *Instances) dropSplitPoints(database string) {
	for name := range s.splitPoints {
		if name == database || strings.HasPrefix(name, database+"/") {
			delete(s.splitPoints, name)
		}
	}
}
