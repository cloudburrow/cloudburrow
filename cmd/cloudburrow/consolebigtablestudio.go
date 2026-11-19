package main

// Bigtable Studio (#1858), an instance's workspace as Google's console has
// it (https://docs.cloud.google.com/bigtable/docs/manage-data-using-console):
// the Explorer (the instance's tables with their column families, their
// authorized views, and the logical and materialized views), the Query
// builder (a table, row-key filters, column filters and a limit, run as
// ReadRows) and the SQL editor (GoogleSQL for Bigtable, run as PrepareQuery
// and ExecuteQuery, which the Bigtable front serves). Its calls are
// POST /api/studio/bigtable-instances?call=explorer|read|sql.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/iterator"
)

type bigtableStudioRequest struct {
	Instance string `json:"instance"`
	Table    string `json:"table"`
	// The query builder's filters.
	Prefix    string `json:"prefix"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Keys      string `json:"keys"`
	Family    string `json:"family"`
	Qualifier string `json:"qualifier"`
	Latest    bool   `json:"latest"`
	Limit     int    `json:"limit"`
	// The SQL editor's statement.
	SQL string `json:"sql"`
}

type bigtableStudioFamily struct {
	Name string `json:"name"`
	GC   string `json:"gc"`
}

type bigtableStudioTable struct {
	Name            string                 `json:"name"`
	Families        []bigtableStudioFamily `json:"families"`
	AuthorizedViews []string               `json:"authorizedViews"`
}

type bigtableStudioExplorer struct {
	Tables            []bigtableStudioTable `json:"tables"`
	LogicalViews      []string              `json:"logicalViews"`
	MaterializedViews []string              `json:"materializedViews"`
	Errors            map[string]string     `json:"errors,omitempty"`
}

// bigtableStudioCell is one cell of a builder result row.
type bigtableStudioCell struct {
	Column    string `json:"column"`
	Value     string `json:"value"`
	Timestamp string `json:"timestamp"`
}

type bigtableStudioRow struct {
	Key   string               `json:"key"`
	Cells []bigtableStudioCell `json:"cells"`
}

type bigtableStudioResult struct {
	Columns []string            `json:"columns"`
	Rows    [][]string          `json:"rows"`
	Details []bigtableStudioRow `json:"details,omitempty"`
	Elapsed string              `json:"elapsed"`
	Message string              `json:"message,omitempty"`
}

// StudioCall implements console.Studio for Bigtable Studio.
func (p bigtableInstancesProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	var req bigtableStudioRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("malformed request: %w", err)
	}
	if req.Instance == "" || strings.ContainsAny(req.Instance, "/ ") {
		return nil, errors.New("a Bigtable Studio call names an instance")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*dbTimeout)
	defer cancel()
	switch call {
	case "explorer":
		return p.studioExplorer(ctx, project, req.Instance)
	case "read":
		return p.studioRead(ctx, project, req)
	case "sql":
		return p.studioSQL(ctx, project, req)
	}
	return nil, fmt.Errorf("unknown Bigtable Studio call %q", call)
}

func (p bigtableInstancesProvider) studioExplorer(ctx context.Context, project, instance string) (*bigtableStudioExplorer, error) {
	out := &bigtableStudioExplorer{Tables: []bigtableStudioTable{}, LogicalViews: []string{}, MaterializedViews: []string{}, Errors: map[string]string{}}
	admin, err := bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Bigtable admin API: %w", err)
	}
	defer admin.Close()
	tables, err := admin.Tables(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	sort.Strings(tables)
	parent := "projects/" + project + "/instances/" + instance
	for _, t := range tables {
		st := bigtableStudioTable{Name: t, Families: []bigtableStudioFamily{}, AuthorizedViews: []string{}}
		if info, err := admin.TableInfo(ctx, t); err == nil {
			for _, f := range info.FamilyInfos {
				gc := f.GCPolicy
				if strings.TrimSpace(gc) == "" {
					gc = "never expires"
				}
				st.Families = append(st.Families, bigtableStudioFamily{Name: f.Name, GC: gc})
			}
			sort.Slice(st.Families, func(i, j int) bool { return st.Families[i].Name < st.Families[j].Name })
		}
		it := admin.TableAdminClientV2().ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: parent + "/tables/" + t})
		for {
			v, err := it.Next()
			if err != nil {
				if err != iterator.Done {
					out.Errors["authorized views of "+t] = err.Error()
				}
				break
			}
			st.AuthorizedViews = append(st.AuthorizedViews, lastSegment(v.GetName()))
		}
		out.Tables = append(out.Tables, st)
	}
	iac, err := p.client(ctx, project)
	if err != nil {
		out.Errors["views"] = err.Error()
		return out, nil
	}
	defer iac.Close()
	if lvs, err := iac.LogicalViews(ctx, instance); err == nil {
		for _, v := range lvs {
			out.LogicalViews = append(out.LogicalViews, lastSegment(v.LogicalViewID))
		}
	} else {
		out.Errors["logical views"] = err.Error()
	}
	if mvs, err := iac.MaterializedViews(ctx, instance); err == nil {
		for _, v := range mvs {
			out.MaterializedViews = append(out.MaterializedViews, lastSegment(v.MaterializedViewID))
		}
	} else {
		out.Errors["materialized views"] = err.Error()
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out, nil
}

// studioRead is the query builder: ReadRows with the row-key filter (a
// prefix, a range, or listed keys), a column filter and a limit.
func (p bigtableInstancesProvider) studioRead(ctx context.Context, project string, req bigtableStudioRequest) (*bigtableStudioResult, error) {
	if req.Table == "" {
		return nil, errors.New("choose a table to query")
	}
	limit := req.Limit
	if limit <= 0 || limit > detailLimit {
		limit = detailLimit
	}
	var rowSet bigtable.RowSet
	switch {
	case strings.TrimSpace(req.Keys) != "":
		rowSet = bigtable.RowList(splitAndTrim(req.Keys))
	case req.Prefix != "":
		rowSet = bigtable.PrefixRange(req.Prefix)
	case req.Start != "" || req.End != "":
		if req.End == "" {
			rowSet = bigtable.InfiniteRange(req.Start)
		} else {
			rowSet = bigtable.NewRange(req.Start, req.End)
		}
	default:
		rowSet = bigtable.InfiniteRange("")
	}
	var filters []bigtable.Filter
	if f := strings.TrimSpace(req.Family); f != "" {
		filters = append(filters, bigtable.FamilyFilter("^"+regexp.QuoteMeta(f)+"$"))
	}
	if q := strings.TrimSpace(req.Qualifier); q != "" {
		filters = append(filters, bigtable.ColumnFilter("^"+regexp.QuoteMeta(q)+"$"))
	}
	if req.Latest {
		filters = append(filters, bigtable.LatestNFilter(1))
	}
	opts := []bigtable.ReadOption{bigtable.LimitRows(int64(limit))}
	switch len(filters) {
	case 0:
	case 1:
		opts = append(opts, bigtable.RowFilter(filters[0]))
	default:
		opts = append(opts, bigtable.RowFilter(bigtable.ChainFilters(filters...)))
	}
	c, err := newConsoleBigtableClient(ctx, p.endpoint, project, req.Instance)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer c.Close()
	out := &bigtableStudioResult{Rows: [][]string{}}
	columns := map[string]bool{}
	start := time.Now()
	err = c.Open(req.Table).ReadRows(ctx, rowSet, func(row bigtable.Row) bool {
		r := bigtableStudioRow{Key: row.Key()}
		for _, family := range sortedFamilies(row) {
			for _, item := range row[family] {
				columns[item.Column] = true
				r.Cells = append(r.Cells, bigtableStudioCell{Column: item.Column, Value: bigtableCellValue(item.Value),
					Timestamp: item.Timestamp.Time().UTC().Format(time.RFC3339Nano)})
			}
		}
		out.Details = append(out.Details, r)
		return true
	}, opts...)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(columns))
	for n := range columns {
		names = append(names, n)
	}
	sort.Strings(names)
	out.Columns = append([]string{"_key"}, names...)
	for _, r := range out.Details {
		latest := map[string]string{}
		for _, cell := range r.Cells {
			if _, ok := latest[cell.Column]; !ok {
				latest[cell.Column] = cell.Value
			}
		}
		line := []string{r.Key}
		for _, n := range names {
			line = append(line, latest[n])
		}
		out.Rows = append(out.Rows, line)
	}
	out.Elapsed = time.Since(start).Round(time.Millisecond).String()
	out.Message = strconv.Itoa(len(out.Rows)) + " rows. Each cell shows its latest version; a row's details list every version."
	return out, nil
}

// studioSQL is the SQL editor: GoogleSQL for Bigtable through PrepareQuery
// and ExecuteQuery.
func (p bigtableInstancesProvider) studioSQL(ctx context.Context, project string, req bigtableStudioRequest) (*bigtableStudioResult, error) {
	sql := strings.TrimSpace(req.SQL)
	if sql == "" {
		return nil, errors.New("the editor is empty: write a query to run")
	}
	start := time.Now()
	c, err := newConsoleBigtableClient(ctx, p.endpoint, project, req.Instance)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer c.Close()
	list, err := bigtableSQLWith(ctx, c, sql)
	if err != nil {
		return nil, err
	}
	if list.Unavailable != "" {
		return nil, errors.New(list.Unavailable)
	}
	out := &bigtableStudioResult{Rows: [][]string{}}
	if list.NameColumn != "" {
		out.Columns = append([]string{list.NameColumn}, list.Columns...)
	}
	for _, it := range list.Items {
		line := []string{it.Name}
		for _, c := range list.Columns {
			line = append(line, it.Fields[c])
		}
		out.Rows = append(out.Rows, line)
	}
	out.Elapsed = time.Since(start).Round(time.Millisecond).String()
	out.Message = strconv.Itoa(len(out.Rows)) + " rows."
	if len(out.Rows) >= detailLimit {
		out.Message = fmt.Sprintf("The first %d rows.", detailLimit)
	}
	return out, nil
}
