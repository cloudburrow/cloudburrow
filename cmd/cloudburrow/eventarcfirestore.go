package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	firestore "cloud.google.com/go/firestore/apiv1"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// feedFirestoreEvents watches each database a Firestore trigger names
// (#1877): the Firestore emulator documents no change notifications, so
// each is watched with Firestore's own Listen, over every collection of
// the database, and each change after the first snapshot is handed to
// Eventarc with the document before and after it. A database no trigger
// names any more stops being watched.
func feedFirestoreEvents(tunnel func() *netfwd.Forwarder, functions func() *runadapter.FunctionsServer, location string) {
	watching := map[runadapter.FirestoreWatch]context.CancelFunc{}
	for range time.Tick(time.Second) {
		f := functions()
		fwd := tunnel()
		if f == nil || fwd == nil || fwd.HostAddr() == "" {
			continue
		}
		want := runadapter.FirestoreWatches()
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
			go watchFirestore(ctx, fwd.HostAddr(), w, func(c runadapter.FirestoreChange) {
				c.Location = location
				ectx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				functions().Eventarc().EmitFirestore(ectx, c)
			})
		}
	}
}

// watchFirestore watches one collection group of a database (every
// collection when w.Collection is "") until ctx ends, reconnecting after
// an error; each new snapshot is compared with what was seen, so a change
// made while it was down is still reported.
func watchFirestore(ctx context.Context, addr string, w runadapter.FirestoreWatch, emit func(runadapter.FirestoreChange)) {
	db := "projects/" + w.Project + "/databases/" + w.Database
	var seen map[string]*firestorepb.Document // nil until the first snapshot
	changed := func(name string, old, cur *firestorepb.Document) {
		emit(runadapter.FirestoreChange{Project: w.Project, Database: w.Database, Name: name, Old: old, New: cur, Time: time.Now()})
	}
	for ctx.Err() == nil {
		var next map[string]*firestorepb.Document
		var err error
		if w.Collection == "" {
			next, err = pollOnce(ctx, addr, db, seen, changed)
		} else {
			next, err = listenOnce(ctx, addr, db, w.Collection, seen, changed)
		}
		if next != nil {
			seen = next
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("eventarc: the Firestore watch for its triggers ended; reconnecting", "database", db, "collection", w.Collection, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// diffDocuments reports what changed from prev to cur.
func diffDocuments(prev, cur map[string]*firestorepb.Document, changed func(name string, old, cur *firestorepb.Document)) {
	for name, d := range cur {
		if old := prev[name]; old == nil || !proto.Equal(old, d) {
			changed(name, old, d)
		}
	}
	for name, old := range prev {
		if _, ok := cur[name]; !ok {
			changed(name, old, nil)
		}
	}
}

func firestoreClient(ctx context.Context, addr string) (*firestore.Client, error) {
	return firestore.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
}

// routed is ctx with the routing headers the Firestore client sends: the
// emulator answers a Listen without them with an empty UNKNOWN.
func routed(ctx context.Context, db string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "google-cloud-resource-prefix", db, "x-goog-request-params", "database="+db)
}

// pollOnce queries every collection of the database each second: the
// emulator's Listen on a query of every collection sends its first
// snapshot and no change after it.
func pollOnce(ctx context.Context, addr, db string, prev map[string]*firestorepb.Document,
	changed func(name string, old, cur *firestorepb.Document)) (map[string]*firestorepb.Document, error) {
	c, err := firestoreClient(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	for {
		stream, err := c.RunQuery(routed(ctx, db), &firestorepb.RunQueryRequest{Parent: db + "/documents",
			QueryType: &firestorepb.RunQueryRequest_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
				From: []*firestorepb.StructuredQuery_CollectionSelector{{AllDescendants: true}}}}})
		if err != nil {
			return prev, err
		}
		cur := map[string]*firestorepb.Document{}
		for {
			r, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return prev, err
			}
			if d := r.GetDocument(); d != nil {
				cur[d.GetName()] = d
			}
		}
		if prev != nil {
			diffDocuments(prev, cur, changed)
		}
		prev = cur
		select {
		case <-ctx.Done():
			return prev, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// listenOnce runs one Listen stream on a collection group and returns the
// documents it last knew of. The emulator answers a change with RESET and
// the target's documents again, so each CURRENT ends a snapshot, which is
// compared with the one before it.
func listenOnce(ctx context.Context, addr, db, collection string, prev map[string]*firestorepb.Document,
	changed func(name string, old, cur *firestorepb.Document)) (map[string]*firestorepb.Document, error) {
	c, err := firestoreClient(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	stream, err := c.Listen(routed(ctx, db))
	if err != nil {
		return nil, err
	}
	const target = 1
	if err := stream.Send(&firestorepb.ListenRequest{Database: db, TargetChange: &firestorepb.ListenRequest_AddTarget{AddTarget: &firestorepb.Target{
		TargetId: target, TargetType: &firestorepb.Target_Query{Query: &firestorepb.Target_QueryTarget{Parent: db + "/documents",
			QueryType: &firestorepb.Target_QueryTarget_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
				From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: collection, AllDescendants: true}}}}}}}}}); err != nil {
		return nil, err
	}
	known := prev // the last consistent snapshot
	cur := map[string]*firestorepb.Document{}
	building := true // a full snapshot is being received, until CURRENT
	settle := func() {
		if known != nil {
			diffDocuments(known, cur, changed)
		}
		known = maps.Clone(cur)
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return known, err
		}
		switch r := resp.GetResponseType().(type) {
		case *firestorepb.ListenResponse_DocumentChange:
			if slices.Contains(r.DocumentChange.GetTargetIds(), target) {
				d := r.DocumentChange.GetDocument()
				cur[d.GetName()] = d
			} else if slices.Contains(r.DocumentChange.GetRemovedTargetIds(), target) {
				delete(cur, r.DocumentChange.GetDocument().GetName())
			}
		case *firestorepb.ListenResponse_DocumentDelete:
			delete(cur, r.DocumentDelete.GetDocument())
		case *firestorepb.ListenResponse_DocumentRemove:
			delete(cur, r.DocumentRemove.GetDocument())
		case *firestorepb.ListenResponse_TargetChange:
			tc := r.TargetChange
			switch {
			case tc.GetTargetChangeType() == firestorepb.TargetChange_REMOVE:
				return known, nil
			case tc.GetTargetChangeType() == firestorepb.TargetChange_RESET:
				// The target's documents follow again, in full.
				building, cur = true, map[string]*firestorepb.Document{}
			case tc.GetTargetChangeType() == firestorepb.TargetChange_CURRENT:
				building = false
				settle()
			case tc.GetTargetChangeType() == firestorepb.TargetChange_NO_CHANGE && len(tc.GetTargetIds()) == 0 && !building:
				// A consistent point after incremental changes.
				settle()
			}
		}
	}
}
