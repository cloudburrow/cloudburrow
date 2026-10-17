package main

// A table page's Change stream section (#1523): the changes of the last
// hour, newest first, read from the table's change stream, which the
// Bigtable front serves from the change log it keeps
// (internal/bigtablefront, changestreams.go). Edit change stream, among the
// table's actions (consolebigtableedit.go), enables it and sets its
// retention.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// changeStreamWindow is how far back the section reads, and
// changeStreamShown how many records it lists at most.
const (
	changeStreamWindow = time.Hour
	changeStreamShown  = 100
)

// describeChangeMutation is one logged mutation as the section shows it.
func describeChangeMutation(m *bigtablepb.Mutation) string {
	switch x := m.GetMutation().(type) {
	case *bigtablepb.Mutation_SetCell_:
		return fmt.Sprintf("SetCell %s:%s = %s", x.SetCell.GetFamilyName(), bigtableCellValue(x.SetCell.GetColumnQualifier()), bigtableCellValue(x.SetCell.GetValue()))
	case *bigtablepb.Mutation_DeleteFromColumn_:
		return fmt.Sprintf("DeleteFromColumn %s:%s", x.DeleteFromColumn.GetFamilyName(), bigtableCellValue(x.DeleteFromColumn.GetColumnQualifier()))
	case *bigtablepb.Mutation_DeleteFromFamily_:
		return "DeleteFromFamily " + x.DeleteFromFamily.GetFamilyName()
	case *bigtablepb.Mutation_AddToCell_:
		return "AddToCell " + x.AddToCell.GetFamilyName()
	case *bigtablepb.Mutation_MergeToCell_:
		return "MergeToCell " + x.MergeToCell.GetFamilyName()
	}
	return fmt.Sprintf("%T", m.GetMutation())
}

func (p bigtableProvider) changeStreamSection(ctx context.Context, project, table string) console.Section {
	out := console.Listing{Columns: []string{"Commit time", "Mutations"}, NameColumn: "Row key", Noun: "changes"}
	sec := console.Section{ID: "changestream", Label: "Change stream", Listing: out}
	if project == "" {
		return sec
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	fail := func(msg string, err error) console.Section {
		out.Unavailable = msg + ": " + err.Error()
		sec.Listing = out
		return sec
	}
	var info *bigtable.TableInfo
	if err := p.admin(ctx, project, func(a *bigtable.AdminClient) (err error) {
		info, err = a.TableInfo(ctx, table)
		return err
	}); err != nil {
		return fail("cannot read the table", err)
	}
	retention, _ := info.ChangeStreamRetention.(time.Duration)
	if retention == 0 {
		out.Note = "This table has no change stream. Edit change stream enables one, kept for 1 to 7 days."
		sec.Listing = out
		return sec
	}
	instance, err := p.instance(ctx, project)
	if err != nil {
		return fail("cannot read the change stream", err)
	}
	conn, err := grpc.NewClient(p.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fail("cannot reach Bigtable", err)
	}
	defer conn.Close()
	d := bigtablepb.NewBigtableClient(conn)
	name := "projects/" + project + "/instances/" + instance + "/tables/" + table
	ps, err := d.GenerateInitialChangeStreamPartitions(ctx, &bigtablepb.GenerateInitialChangeStreamPartitionsRequest{TableName: name})
	if err != nil {
		return fail("cannot read the change stream's partitions", err)
	}
	var partitions []*bigtablepb.StreamPartition
	for {
		r, err := ps.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("cannot read the change stream's partitions", err)
		}
		partitions = append(partitions, r.GetPartition())
	}
	if len(partitions) != 1 {
		return fail("cannot read the change stream", fmt.Errorf("%d partitions; the section reads one", len(partitions)))
	}
	now := time.Now()
	start := now.Add(-min(changeStreamWindow, retention))
	s, err := d.ReadChangeStream(ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name,
		Partition: partitions[0],
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)},
		EndTime:   timestamppb.New(now)})
	if err != nil {
		return fail("cannot read the change stream", err)
	}
	var items []console.Resource
	var cur *console.Resource
	var muts []string
	for {
		r, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("cannot read the change stream", err)
		}
		dc := r.GetDataChange()
		if dc == nil {
			continue
		}
		if cur == nil {
			cur = &console.Resource{Name: bigtableCellValue(dc.GetRowKey()), Fields: map[string]string{
				"Commit time": dc.GetCommitTimestamp().AsTime().UTC().Format(time.RFC3339Nano)}}
			muts = nil
		}
		for _, ch := range dc.GetChunks() {
			if ci := ch.GetChunkInfo(); ci != nil && ci.GetChunkedValueOffset() > 0 {
				continue
			}
			muts = append(muts, describeChangeMutation(ch.GetMutation()))
		}
		if dc.GetDone() {
			cur.Fields["Mutations"] = strings.Join(muts, "; ")
			items = append(items, *cur)
			cur = nil
		}
	}
	out.Total = len(items)
	for i := len(items) - 1; i >= 0 && len(out.Items) < changeStreamShown; i-- {
		out.Items = append(out.Items, items[i])
	}
	out.Note = fmt.Sprintf("The changes of the last %s, newest first (at most %d), of a change stream kept for %s.",
		min(changeStreamWindow, retention), changeStreamShown, formatChangeStreamRetentionNote(retention))
	sec.Listing = out
	return sec
}

func formatChangeStreamRetentionNote(d time.Duration) string {
	if v := formatChangeStreamRetention(d); !strings.ContainsAny(v, "hms") {
		if v == "1" {
			return "1 day"
		}
		return v + " days"
	}
	return d.String()
}
