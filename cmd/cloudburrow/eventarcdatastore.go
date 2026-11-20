package main

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	datastorepb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// feedDatastoreEvents watches each database a Datastore mode trigger
// names (#1877): the Datastore emulator documents no change notifications,
// so every kind of every namespace is queried each second and each change
// after the first snapshot is handed to Eventarc with the entity before
// and after it. A database no trigger names any more stops being watched.
func feedDatastoreEvents(tunnel func() *netfwd.Forwarder, functions func() *runadapter.FunctionsServer, location string) {
	watching := map[runadapter.DatastoreWatch]context.CancelFunc{}
	for range time.Tick(time.Second) {
		f := functions()
		fwd := tunnel()
		if f == nil || fwd == nil || fwd.HostAddr() == "" {
			continue
		}
		want := runadapter.DatastoreWatches()
		for w, cancel := range watching {
			if !slices.Contains(want, w) {
				cancel()
				delete(watching, w)
			}
		}
		for _, w := range want {
			if _, ok := watching[w]; ok {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			watching[w] = cancel
			go watchDatastore(ctx, fwd.HostAddr(), w, func(c runadapter.DatastoreChange) {
				c.Location = location
				ectx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				functions().Eventarc().EmitDatastore(ectx, c)
			})
		}
	}
}

// watchDatastore polls one database until ctx ends, reconnecting after an
// error; each snapshot is compared with the one before it, so a change
// made while it was down is still reported.
func watchDatastore(ctx context.Context, addr string, w runadapter.DatastoreWatch, emit func(runadapter.DatastoreChange)) {
	db := w.Database
	if db == "(default)" {
		db = ""
	}
	var prev map[string]*datastorepb.EntityResult // nil until the first snapshot
	for ctx.Err() == nil {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		c := datastorepb.NewDatastoreClient(conn)
		for err == nil && ctx.Err() == nil {
			var cur map[string]*datastorepb.EntityResult
			cur, err = datastoreSnapshot(ctx, c, w.Project, db)
			if err != nil {
				break
			}
			if prev != nil {
				diffEntities(prev, cur, func(old, next *datastorepb.EntityResult) {
					emit(runadapter.DatastoreChange{Project: w.Project, Database: w.Database, Old: old, New: next, Time: time.Now()})
				})
			}
			prev = cur
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		if conn != nil {
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("eventarc: the Datastore watch for its triggers ended; reconnecting", "project", w.Project, "database", w.Database, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// datastoreSnapshot is every entity of every user kind in every namespace
// of the database, by its key.
func datastoreSnapshot(ctx context.Context, c datastorepb.DatastoreClient, project, db string) (map[string]*datastorepb.EntityResult, error) {
	if db != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-goog-request-params",
			"project_id="+url.QueryEscape(project)+"&database_id="+url.QueryEscape(db))
	}
	run := func(ns string, q *datastorepb.Query) ([]*datastorepb.EntityResult, error) {
		var out []*datastorepb.EntityResult
		for {
			resp, err := c.RunQuery(ctx, &datastorepb.RunQueryRequest{ProjectId: project, DatabaseId: db,
				PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: db, NamespaceId: ns},
				QueryType:   &datastorepb.RunQueryRequest_Query{Query: q}})
			if err != nil {
				return nil, err
			}
			b := resp.GetBatch()
			out = append(out, b.GetEntityResults()...)
			if b.GetMoreResults() != datastorepb.QueryResultBatch_NOT_FINISHED || len(b.GetEntityResults()) == 0 {
				return out, nil
			}
			q = proto.Clone(q).(*datastorepb.Query)
			q.StartCursor = b.GetEndCursor()
		}
	}
	namespaces, err := run("", &datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: "__namespace__"}},
		Projection: []*datastorepb.Projection{{Property: &datastorepb.PropertyReference{Name: "__key__"}}}})
	if err != nil {
		return nil, err
	}
	nss := []string{""}
	for _, r := range namespaces {
		if p := r.GetEntity().GetKey().GetPath(); len(p) == 1 && p[0].GetName() != "" {
			nss = append(nss, p[0].GetName())
		}
	}
	out := map[string]*datastorepb.EntityResult{}
	for _, ns := range slices.Compact(nss) {
		kinds, err := run(ns, &datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: "__kind__"}},
			Projection: []*datastorepb.Projection{{Property: &datastorepb.PropertyReference{Name: "__key__"}}}})
		if err != nil {
			return nil, err
		}
		for _, k := range kinds {
			p := k.GetEntity().GetKey().GetPath()
			if len(p) != 1 || strings.HasPrefix(p[0].GetName(), "__") {
				continue
			}
			rs, err := run(ns, &datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: p[0].GetName()}}})
			if err != nil {
				return nil, err
			}
			for _, r := range rs {
				out[ns+"\x00"+runadapter.EntityPath(r.GetEntity().GetKey())] = r
			}
		}
	}
	return out, nil
}

// diffEntities reports what changed from prev to cur: an entity whose
// properties changed (not only its version) is an update.
func diffEntities(prev, cur map[string]*datastorepb.EntityResult, changed func(old, cur *datastorepb.EntityResult)) {
	for k, r := range cur {
		if old := prev[k]; old == nil || !proto.Equal(old.GetEntity(), r.GetEntity()) {
			changed(old, r)
		}
	}
	for k, old := range prev {
		if _, ok := cur[k]; !ok {
			changed(old, nil)
		}
	}
}
