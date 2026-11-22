package spannerfront

// Serving PLAN, PROFILE and WITH_PLAN_AND_STATS (#2029): the schema the
// plan builder (plan.go) reads, and the statistics PROFILE returns.
//
// Each statistic is measured, none is set: an operator's rows are counted
// by the engine on the operator's part of the query (its FROM items, the
// conditions applied up to it, its GROUP BY or LIMIT), with the query's
// parameters, in the query's own transaction; a scan's rows are its rows
// scanned. An operator's latency is the engine's time to produce its part,
// less its children's; its CPU time is that latency, since the engine
// evaluates a query in one thread, in memory. The query's elapsed_time is
// the time from the request to its last row, its cpu_time the engine's own
// time running it, its rows_returned and rows_scanned the sums the plan
// counts; query_plan_creation_time is the time the plan took to build.
// The engine runs every table in one split, so a Distributed Union makes
// one local call and no remote one.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

var planSchemaSQL = map[bool][]string{
	false: {
		"SELECT TABLE_NAME, IFNULL(PARENT_TABLE_NAME, '') FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = '' AND TABLE_TYPE = 'BASE TABLE'",
		"SELECT TABLE_NAME, COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = '' ORDER BY TABLE_NAME, ORDINAL_POSITION",
		"SELECT TABLE_NAME, INDEX_NAME, INDEX_TYPE, COLUMN_NAME, IFNULL(COLUMN_ORDERING, ''), IFNULL(CAST(ORDINAL_POSITION AS STRING), '') " +
			"FROM INFORMATION_SCHEMA.INDEX_COLUMNS WHERE TABLE_SCHEMA = '' ORDER BY TABLE_NAME, INDEX_NAME, ORDINAL_POSITION",
		"SELECT TABLE_NAME, INDEX_NAME, IFNULL(PARENT_TABLE_NAME, '') FROM INFORMATION_SCHEMA.INDEXES WHERE TABLE_SCHEMA = '' AND INDEX_TYPE = 'INDEX'",
		"SELECT TABLE_NAME, IFNULL(VIEW_DEFINITION, '') FROM INFORMATION_SCHEMA.VIEWS WHERE TABLE_SCHEMA = ''",
	},
	true: {
		"SELECT table_name, COALESCE(parent_table_name, '') FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'",
		"SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = 'public' ORDER BY table_name, ordinal_position",
		"SELECT table_name, index_name, index_type, column_name, COALESCE(column_ordering, ''), COALESCE(CAST(ordinal_position AS varchar), '') " +
			"FROM information_schema.index_columns WHERE table_schema = 'public' ORDER BY table_name, index_name, ordinal_position",
		"SELECT table_name, index_name, COALESCE(parent_table_name, '') FROM information_schema.indexes WHERE table_schema = 'public' AND index_type = 'INDEX'",
		"SELECT table_name, COALESCE(view_definition, '') FROM information_schema.views WHERE table_schema = 'public'",
	},
}

// engineSQL runs one statement on the engine and returns its rows as
// strings, and the engine's own time for it.
func (s *Instances) engineSQL(ctx context.Context, session string, tx *spannerpb.TransactionSelector, sql string, params *structpb.Struct, types map[string]*spannerpb.Type) ([][]string, time.Duration, error) {
	rs, err := s.data.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: session, Transaction: tx, Sql: sql,
		Params: params, ParamTypes: types, QueryMode: spannerpb.ExecuteSqlRequest_PROFILE})
	if err != nil {
		return nil, 0, err
	}
	var out [][]string
	for _, r := range rs.GetRows() {
		var row []string
		for _, v := range r.GetValues() {
			switch x := v.GetKind().(type) {
			case *structpb.Value_StringValue:
				row = append(row, x.StringValue)
			case *structpb.Value_NumberValue:
				row = append(row, strconv.FormatFloat(x.NumberValue, 'f', -1, 64))
			case *structpb.Value_BoolValue:
				row = append(row, strconv.FormatBool(x.BoolValue))
			default:
				row = append(row, "")
			}
		}
		out = append(out, row)
	}
	return out, engineElapsed(rs.GetStats()), nil
}

// engineElapsed is the engine's elapsed_time statistic.
func engineElapsed(st *spannerpb.ResultSetStats) time.Duration {
	v := st.GetQueryStats().GetFields()["elapsed_time"].GetStringValue()
	d, err := time.ParseDuration(strings.ReplaceAll(v, " ", ""))
	if err != nil {
		return 0
	}
	return d
}

func singleUse() *spannerpb.TransactionSelector {
	return &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_SingleUse{SingleUse: &spannerpb.TransactionOptions{
		Mode: &spannerpb.TransactionOptions_ReadOnly_{ReadOnly: &spannerpb.TransactionOptions_ReadOnly{
			TimestampBound: &spannerpb.TransactionOptions_ReadOnly_Strong{Strong: true}}}}}}
}

// readPlanSchema reads the database's tables, columns, keys and indexes.
func (s *Instances) readPlanSchema(ctx context.Context, session string) (*planSchema, error) {
	db, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: databaseOfSession(session)})
	if err != nil {
		return nil, err
	}
	pg := db.GetDatabaseDialect() == databasepb.DatabaseDialect_POSTGRESQL
	sc := &planSchema{pg: pg, tables: map[string]*planTable{}, indexes: map[string]*planIndex{}, views: map[string]string{}}
	var parts [5][][]string
	for i, q := range planSchemaSQL[pg] {
		rows, _, err := s.engineSQL(ctx, session, singleUse(), q, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("reading the schema: %w", err)
		}
		parts[i] = rows
	}
	for _, r := range parts[0] {
		sc.tables[strings.ToLower(r[0])] = &planTable{name: r[0], parent: r[1]}
	}
	for _, r := range parts[1] {
		if t := sc.tables[strings.ToLower(r[0])]; t != nil {
			t.cols = append(t.cols, r[1])
		}
	}
	for _, r := range parts[3] {
		sc.indexes[strings.ToLower(r[1])] = &planIndex{name: r[1], table: r[0], parent: r[2]}
	}
	for _, r := range parts[2] {
		k := keyCol{name: r[3], desc: r[4] == "DESC"}
		switch {
		case r[2] == "PRIMARY_KEY" && r[5] != "":
			if t := sc.tables[strings.ToLower(r[0])]; t != nil {
				t.pk = append(t.pk, k)
			}
		case r[2] == "INDEX":
			ix := sc.indexes[strings.ToLower(r[1])]
			if ix == nil {
				continue
			}
			if r[5] == "" {
				ix.storing = append(ix.storing, r[3])
			} else {
				ix.keys = append(ix.keys, k)
			}
		}
	}
	for _, r := range parts[4] {
		sc.views[strings.ToLower(r[0])] = r[1]
	}
	return sc, nil
}

// readGraphs reads the database's property graphs.
func (s *Instances) readGraphs(ctx context.Context, session string) (map[string]*graphMeta, error) {
	rows, _, err := s.engineSQL(ctx, session, singleUse(),
		"SELECT PROPERTY_GRAPH_NAME, TO_JSON_STRING(PROPERTY_GRAPH_METADATA_JSON) FROM INFORMATION_SCHEMA.PROPERTY_GRAPHS WHERE PROPERTY_GRAPH_SCHEMA = ''", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the property graphs: %w", err)
	}
	out := map[string]*graphMeta{}
	for _, r := range rows {
		g, err := parseGraphMeta(r[1])
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(r[0])] = g
	}
	return out, nil
}

// builtPlan is a query's plan, ready to serve.
type builtPlan struct {
	root    *planOp
	nodes   []*spannerpb.PlanNode
	byOp    map[*planOp]*spannerpb.PlanNode
	created time.Duration
}

// buildQueryPlan plans req's query. The engine has already validated it.
func (s *Instances) buildQueryPlan(ctx context.Context, req *spannerpb.ExecuteSqlRequest) (*builtPlan, error) {
	start := time.Now()
	sc, err := s.readPlanSchema(ctx, req.GetSession())
	if err != nil {
		return nil, err
	}
	src := req.GetSql()
	var quant map[string]*graphQuant
	if !sc.pg && mentionsGraph(src) {
		// a graph query is planned as the SQL query that reads its rows
		// (plangraph.go); property graphs are GoogleSQL's only
		graphs, gerr := s.readGraphs(ctx, req.GetSession())
		if gerr != nil {
			return nil, gerr
		}
		src, quant, err = graphToSQLQuant(src, graphs)
	}
	var ps *sqlParser
	var stmt *sqlStmt
	if err == nil {
		ps, stmt, err = parseStatementSQL(src, sc.pg)
	}
	if err == nil {
		b := &planBuilder{ps: ps, schema: sc, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
			subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}, quant: quant}
		var root *planOp
		if stmt.dml != nil {
			root, err = b.buildDML(stmt.dml)
		} else {
			root, err = b.buildPlan(stmt.query)
		}
		if err == nil {
			nodes, byOp := planNodes(root)
			return &builtPlan{root: root, nodes: nodes, byOp: byOp, created: time.Since(start)}, nil
		}
	}
	if u, ok := err.(unsupportedSQL); ok {
		return nil, status.Errorf(codes.Unimplemented, "Query mode %s is not served for this query: CloudBurrow builds a query's plan from the query and the schema (the Spanner emulator has none), and its planner does not plan %s yet. Run the query in NORMAL or WITH_STATS mode.", req.GetQueryMode(), u.what)
	}
	return nil, status.Errorf(codes.Internal, "planning the query: %v", err)
}

// planStats are what a run of the plan measured.
type planStats struct {
	rows, execs map[*planOp]int64
	cum         map[*planOp]time.Duration
}

// measure counts each operator's rows on the engine.
func (s *Instances) measure(ctx context.Context, p *builtPlan, req *spannerpb.ExecuteSqlRequest, tx *spannerpb.TransactionSelector, returned int64) (*planStats, error) {
	st := &planStats{rows: map[*planOp]int64{}, execs: map[*planOp]int64{}, cum: map[*planOp]time.Duration{}}
	count := func(sql string) (int64, time.Duration, error) {
		rows, d, err := s.engineSQL(ctx, req.GetSession(), tx, sql, req.GetParams(), req.GetParamTypes())
		if err != nil {
			return 0, 0, fmt.Errorf("counting an operator's rows (%s): %w", sql, err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			return 0, d, nil
		}
		f, err := strconv.ParseFloat(rows[0][0], 64)
		if err != nil {
			return 0, d, nil
		}
		return int64(f), d, nil
	}
	var walk func(o *planOp) error
	walk = func(o *planOp) error {
		var kidsCum time.Duration
		for _, l := range o.kids {
			if l.rel != nil {
				if err := walk(l.rel); err != nil {
					return err
				}
				kidsCum = max(kidsCum, st.cum[l.rel])
			}
			if l.sc != nil {
				if err := walkScalarSubplans(l.sc, walk); err != nil {
					return err
				}
			}
		}
		st.cum[o] = kidsCum
		switch {
		case o == p.root && returned >= 0, o.rows == rowsReturned:
			st.rows[o] = max(returned, 0)
		case o.rows != "":
			n, d, err := count(o.rows)
			if err != nil {
				return err
			}
			st.rows[o] = n
			st.cum[o] = max(d, kidsCum)
		case o.firstRel() != nil:
			st.rows[o] = st.rows[o.firstRel()]
		}
		st.execs[o] = 1
		if o.execs != "" {
			n, _, err := count(o.execs)
			if err != nil {
				return err
			}
			st.execs[o] = n
		}
		return nil
	}
	if err := walk(p.root); err != nil {
		return nil, err
	}
	return st, nil
}

func walkScalarSubplans(sc *planScalar, walk func(*planOp) error) error {
	for _, l := range sc.kids {
		if l.rel != nil {
			if err := walk(l.rel); err != nil {
				return err
			}
		}
		if l.sc != nil {
			if err := walkScalarSubplans(l.sc, walk); err != nil {
				return err
			}
		}
	}
	return nil
}

func msecs(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 2, 64)
}

func statValue(total, unit string) map[string]any {
	return map[string]any{"total": total, "unit": unit}
}

// applyExecutionStats sets each relational node's executionStats.
func applyExecutionStats(p *builtPlan, st *planStats) int64 {
	var scanned int64
	for o, n := range p.byOp {
		var kids time.Duration
		for _, l := range o.kids {
			if l.rel != nil {
				kids += st.cum[l.rel]
			}
		}
		own := st.cum[o] - kids
		if own < 0 || strings.HasPrefix(o.name, "Distributed") {
			// a distributed operator's latency is cumulative (Google's
			// distributed operators page)
			own = st.cum[o]
		}
		m := map[string]any{
			"rows":              statValue(strconv.FormatInt(st.rows[o], 10), "rows"),
			"latency":           statValue(msecs(own), "msecs"),
			"cpu_time":          statValue(msecs(own), "msecs"),
			"execution_summary": map[string]any{"num_executions": strconv.FormatInt(st.execs[o], 10)},
		}
		if o.scan && o.meta["scan_type"] != "BatchScan" {
			m["scanned_rows"] = statValue(strconv.FormatInt(st.rows[o], 10), "rows")
			scanned += st.rows[o]
		}
		if o.filtered {
			if in := o.firstRel(); in != nil {
				m["filtered_rows"] = statValue(strconv.FormatInt(st.rows[in]-st.rows[o], 10), "rows")
			}
		}
		if o.name == "Distributed Union" || strings.HasPrefix(o.name, "Distributed ") {
			m["remote_calls"] = statValue("0", "calls")
		}
		n.ExecutionStats, _ = structpb.NewStruct(m)
	}
	return scanned
}

// queryStats are the query's statistics.
func queryStats(req *spannerpb.ExecuteSqlRequest, p *builtPlan, elapsed, engine time.Duration, returned, scanned int64, withScanned bool) *structpb.Struct {
	m := map[string]any{
		"query_text":               req.GetSql(),
		"elapsed_time":             msecs(elapsed) + " msecs",
		"cpu_time":                 msecs(min(engine, elapsed)) + " msecs",
		"rows_returned":            strconv.FormatInt(returned, 10),
		"query_plan_creation_time": msecs(p.created) + " msecs",
	}
	if withScanned {
		m["rows_scanned"] = strconv.FormatInt(scanned, 10)
	}
	st, _ := structpb.NewStruct(m)
	return st
}

// isDML says whether sql is an INSERT, UPDATE or DELETE.
// forUpdate says whether a statement has a FOR UPDATE clause.
func forUpdate(sql string) bool {
	toks, err := lexSQL(sql, false)
	if err != nil {
		return false
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind == tkIdent && strings.EqualFold(toks[i].text, "FOR") && toks[i+1].kind == tkIdent && strings.EqualFold(toks[i+1].text, "UPDATE") {
			return true
		}
	}
	return false
}

func isDML(sql string) bool {
	toks, err := lexSQL(sql, false)
	if err != nil {
		toks, err = lexSQL(sql, true)
	}
	if err != nil {
		return false
	}
	i := 0
	if len(toks) > 0 && toks[0].kind == tkPunct && toks[0].text == "@{" {
		for i < len(toks) && !(toks[i].kind == tkPunct && toks[i].text == "}") {
			i++
		}
		i++
	}
	for i < len(toks) && toks[i].kind == tkPGHint {
		i++
	}
	if i >= len(toks) || toks[i].kind != tkIdent {
		return false
	}
	switch strings.ToUpper(toks[i].text) {
	case "INSERT", "UPDATE", "DELETE":
		return true
	}
	return false
}

// servePlanned serves a statement in a mode with a plan. run runs a
// request on the engine and sends its answers on, all but the last, with
// began, when set, as the first answer's transaction; it returns that last
// answer's statistics and the transaction the request began, if any.
// servePlanned returns the statistics to put on the last answer.
//
// In PLAN mode the engine validates the statement, which runs nothing, and
// the front plans it. In PROFILE and WITH_PLAN_AND_STATS modes the engine
// validates it first, so that a statement the front cannot plan is refused
// before it runs; the front counts the plan's rows; then the engine runs
// it. A DML statement's rows are counted before it changes them, in its own
// read-write transaction, which the front begins when the request asks
// for one to be begun.
func (s *Instances) servePlanned(ctx context.Context, req *spannerpb.ExecuteSqlRequest,
	run func(r *spannerpb.ExecuteSqlRequest, began *spannerpb.Transaction) (*spannerpb.ResultSetStats, []byte, error)) (*spannerpb.ResultSetStats, error) {
	start := time.Now()
	if req.GetQueryMode() == spannerpb.ExecuteSqlRequest_PLAN {
		estats, _, err := run(req, nil)
		if err != nil {
			return nil, err
		}
		p, err := s.buildQueryPlan(ctx, req)
		if err != nil {
			return nil, err
		}
		return &spannerpb.ResultSetStats{QueryPlan: &spannerpb.QueryPlan{PlanNodes: p.nodes}, RowCount: estats.GetRowCount()}, nil
	}
	dml := isDML(req.GetSql())
	tx := req.GetTransaction()
	var began *spannerpb.Transaction
	// a DML statement, or a query that locks (FOR UPDATE, which a
	// read-only transaction refuses), is validated and counted in its own
	// read-write transaction
	if (dml || forUpdate(req.GetSql())) && tx.GetBegin() != nil {
		t, err := s.data.BeginTransaction(ctx, &spannerpb.BeginTransactionRequest{Session: req.GetSession(), Options: tx.GetBegin()})
		if err != nil {
			return nil, err
		}
		began = t
		tx = &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Id{Id: t.GetId()}}
	}
	check := proto.Clone(req).(*spannerpb.ExecuteSqlRequest)
	check.QueryMode = spannerpb.ExecuteSqlRequest_PLAN
	// the statement's own seqno is for its run: a transaction takes one
	// request per seqno
	check.Seqno = 0
	switch {
	case tx.GetId() != nil:
		check.Transaction = tx
	case !dml:
		// validating begins no transaction
		check.Transaction = singleUse()
	}
	if _, err := s.data.ExecuteSql(ctx, check); err != nil {
		return nil, err
	}
	p, err := s.buildQueryPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	var st *planStats
	if dml {
		if st, err = s.measure(ctx, p, req, tx, -1); err != nil {
			return nil, err
		}
	}
	exec := proto.Clone(req).(*spannerpb.ExecuteSqlRequest)
	exec.QueryMode = spannerpb.ExecuteSqlRequest_PROFILE
	exec.Transaction = tx
	estats, beganID, err := run(exec, began)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)
	returned, _ := strconv.ParseInt(estats.GetQueryStats().GetFields()["rows_returned"].GetStringValue(), 10, 64)
	if !dml {
		switch {
		case beganID != nil:
			tx = &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Id{Id: beganID}}
		case tx == nil || tx.GetSelector() == nil:
			tx = singleUse()
		}
		if st, err = s.measure(ctx, p, req, tx, returned); err != nil {
			return nil, err
		}
	} else {
		for o := range p.byOp {
			if o.affected {
				st.rows[o] = estats.GetRowCountExact()
			}
		}
		if p.root.name == "Serialize Result" {
			st.rows[p.root] = returned
		}
	}
	scanned := applyExecutionStats(p, st)
	if req.GetQueryMode() == spannerpb.ExecuteSqlRequest_WITH_PLAN_AND_STATS {
		// the overall statistics, not the operators' (spanner.proto)
		for _, n := range p.nodes {
			n.ExecutionStats = nil
		}
	}
	return &spannerpb.ResultSetStats{QueryPlan: &spannerpb.QueryPlan{PlanNodes: p.nodes}, RowCount: estats.GetRowCount(),
		QueryStats: queryStats(req, p, elapsed, engineElapsed(estats), returned, scanned, true)}, nil
}
