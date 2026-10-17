package firestoreadmin

// Firestore pipelines (google.firestore.v1.Firestore/ExecutePipeline,
// #1500). The emulator refuses them: pipelines are a Firestore Enterprise
// edition feature, and the emulator is the Standard edition. The front
// runs them itself, following Google's rule: on a database created with
// the ENTERPRISE edition they are evaluated here, and on a Standard one
// (`(default)` included) they are refused INVALID_ARGUMENT with Google's
// message, as on Google.
//
// The input stage's documents are read from the emulator over its own
// gRPC API (readAll: ListCollectionIds and ListDocuments), and every later
// stage is evaluated in Go over them, in the order given. The stages and
// functions served are those of Google's pipeline operations reference
// (pipelineStages, pipelineFunctions); every other one is refused naming
// it: UNIMPLEMENTED for one Google documents that is not served here,
// INVALID_ARGUMENT for a name Google does not know.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

const (
	pipelineMethod = "/google.firestore.v1.Firestore/ExecutePipeline"
	// pipelineBatch is how many results one response carries.
	pipelineBatch = 100
)

var executePipelinePath = regexp.MustCompile(`^/v1/(projects/[^/]+/databases/[^/]+)/documents:executePipeline$`)

// pipelineServer is the documents API's ExecutePipeline alone; every other
// documents call goes to the emulator.
type pipelineServer struct {
	firestorepb.UnimplementedFirestoreServer
	a *Admin
}

func (s pipelineServer) ExecutePipeline(req *firestorepb.ExecutePipelineRequest, stream firestorepb.Firestore_ExecutePipelineServer) error {
	resps, err := s.a.executePipeline(stream.Context(), req)
	if err != nil {
		return err
	}
	for _, r := range resps {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

// servePipelineREST answers POST .../documents:executePipeline, whose
// response, a server stream over REST, is a JSON array of the responses.
func (a *Admin) servePipelineREST(w http.ResponseWriter, r *http.Request, db string) {
	if r.Method != http.MethodPost {
		writeError(w, status.Errorf(codes.Unimplemented, "%s %s is not served: executePipeline is POST", r.Method, r.URL.Path))
		return
	}
	req := &firestorepb.ExecutePipelineRequest{}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err == nil && len(b) > 0 {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, req)
	}
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "invalid JSON payload: %v", err))
		return
	}
	req.Database = db
	resps, err := a.executePipeline(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	parts := make([]json.RawMessage, 0, len(resps))
	for _, m := range resps {
		p, err := protojson.Marshal(m)
		if err != nil {
			writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
			return
		}
		parts = append(parts, p)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(parts)
}

// --- the request ---

// pipelineStages are the stages Google's pipeline operations reference
// documents, and whether each is served here.
var pipelineStages = map[string]bool{
	"collection": true, "collection_group": true, "database": true, "documents": true,
	"where": true, "select": true, "add_fields": true, "remove_fields": true,
	"sort": true, "limit": true, "offset": true, "aggregate": true, "distinct": true,
	"find_nearest": true, "sample": true, "union": true, "unnest": true, "replace_with": true,

	// Not served: full-text search needs a search index, the DML stages
	// write, and literals and let are not in the reference's stage list.
	"search": false, "update": false, "delete": false, "literals": false, "let": false, "subcollection": false,
}

func (a *Admin) executePipeline(ctx context.Context, req *firestorepb.ExecutePipelineRequest) ([]*firestorepb.ExecutePipelineResponse, error) {
	a.mu.Lock()
	db, err := a.database(req.GetDatabase())
	var edition adminpb.Database_DatabaseEdition
	if err == nil {
		edition = db.GetDatabaseEdition()
	}
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if edition != adminpb.Database_ENTERPRISE {
		return nil, status.Error(codes.InvalidArgument, "ExecutePipeline requires the Database Edition to be `enterprise`.")
	}
	switch req.GetConsistencySelector().(type) {
	case *firestorepb.ExecutePipelineRequest_Transaction, *firestorepb.ExecutePipelineRequest_NewTransaction:
		return nil, status.Error(codes.Unimplemented, "ExecutePipeline in a transaction is not served here: the front reads the emulator's documents outside its transactions")
	case *firestorepb.ExecutePipelineRequest_ReadTime:
		return nil, status.Error(codes.Unimplemented, "ExecutePipeline at a read_time is not served here: the front reads the emulator's documents as they are now")
	}
	sp := req.GetStructuredPipeline()
	if sp == nil || len(sp.GetPipeline().GetStages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "structured_pipeline with at least one stage is required.")
	}
	explain, format := "", "text"
	for k, v := range sp.GetOptions() {
		switch k {
		case "explain_options":
			for ek, ev := range v.GetMapValue().GetFields() {
				switch ek {
				case "mode":
					explain = ev.GetStringValue()
					if explain != "analyze" && explain != "explain" {
						return nil, status.Errorf(codes.InvalidArgument, "explain_options.mode %q is invalid: want analyze or explain.", explain)
					}
				case "output_format":
					format = strings.ToLower(ev.GetStringValue())
					if format != "text" && format != "json" {
						return nil, status.Errorf(codes.InvalidArgument, "explain_options.output_format %q is invalid: want text or json.", ev.GetStringValue())
					}
				default:
					return nil, status.Errorf(codes.InvalidArgument, "unknown explain option %q.", ek)
				}
			}
		case "index_mode":
			// Every pipeline runs without an index here, as every query
			// does on the emulator; the mode changes nothing.
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown pipeline option %q.", k)
		}
	}
	now := a.now()
	start := time.Now()
	ev := &pipelineRun{a: a, ctx: ctx, db: req.GetDatabase(), now: now}
	rows, err := ev.run(sp.GetPipeline())
	if err != nil {
		return nil, err
	}
	exec := timestamppb.New(now)
	var out []*firestorepb.ExecutePipelineResponse
	if explain != "explain" {
		for i := 0; i < len(rows); i += pipelineBatch {
			resp := &firestorepb.ExecutePipelineResponse{ExecutionTime: exec}
			for _, r := range rows[i:min(i+pipelineBatch, len(rows))] {
				resp.Results = append(resp.Results, r.document())
			}
			out = append(out, resp)
		}
	}
	if len(out) == 0 {
		out = append(out, &firestorepb.ExecutePipelineResponse{ExecutionTime: exec})
	}
	if explain != "" {
		data, err := anypb.New(wrapperspb.String(ev.explainText(sp.GetPipeline(), explain, format, len(rows), time.Since(start))))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encoding the explain stats: %v", err)
		}
		out[len(out)-1].ExplainStats = &firestorepb.ExplainStats{Data: data}
	}
	return out, nil
}

// --- rows ---

// row is a document flowing through the pipeline. name, create and update
// are its identity, kept until a stage builds new rows (select, aggregate,
// distinct).
type row struct {
	name           string
	create, update *timestamppb.Timestamp
	fields         map[string]*firestorepb.Value
}

func (r row) document() *firestorepb.Document {
	return &firestorepb.Document{Name: r.name, Fields: r.fields, CreateTime: r.create, UpdateTime: r.update}
}

func (r row) clone() row {
	c := r
	c.fields = make(map[string]*firestorepb.Value, len(r.fields))
	for k, v := range r.fields {
		c.fields[k] = v
	}
	return c
}

type pipelineRun struct {
	a     *Admin
	ctx   context.Context
	db    string
	now   time.Time
	docs  []*firestorepb.Document // the database's documents, read once
	read  bool
	stats []string
}

func (p *pipelineRun) all() ([]*firestorepb.Document, error) {
	if !p.read {
		docs, err := p.a.readAll(p.ctx, p.db)
		if err != nil {
			return nil, err
		}
		p.docs, p.read = docs, true
	}
	return p.docs, nil
}

func docRow(d *firestorepb.Document) row {
	return row{name: d.Name, create: d.CreateTime, update: d.UpdateTime, fields: d.Fields}
}

// run evaluates the stages of pl in order.
func (p *pipelineRun) run(pl *firestorepb.Pipeline) ([]row, error) {
	var rows []row
	for i, st := range pl.GetStages() {
		name := st.GetName()
		served, known := pipelineStages[name]
		if !known {
			return nil, status.Errorf(codes.InvalidArgument, "Unknown pipeline stage %q.", name)
		}
		if !served {
			return nil, status.Errorf(codes.Unimplemented, "the pipeline stage %q is not served here: CloudBurrow evaluates pipelines itself, and does not have it", name)
		}
		input := name == "collection" || name == "collection_group" || name == "database" || name == "documents"
		if input != (i == 0) {
			if input {
				return nil, status.Errorf(codes.InvalidArgument, "the input stage %q must be the pipeline's first stage.", name)
			}
			return nil, status.Errorf(codes.InvalidArgument, "a pipeline must start with an input stage (collection, collection_group, database or documents), not %q.", name)
		}
		var err error
		rows, err = p.stage(st, rows)
		if err != nil {
			return nil, err
		}
		p.stats = append(p.stats, fmt.Sprintf("%s: %d", name, len(rows)))
	}
	return rows, nil
}

func stageErr(stage, format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, "%s: %s", stage, fmt.Sprintf(format, args...))
}

func (p *pipelineRun) stage(st *firestorepb.Pipeline_Stage, rows []row) ([]row, error) {
	name, args, opts := st.GetName(), st.GetArgs(), st.GetOptions()
	arg := func(i int) *firestorepb.Value {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	switch name {
	case "collection":
		path := strings.Trim(arg(0).GetReferenceValue(), "/")
		if path == "" || strings.Count(path, "/")%2 != 0 {
			return nil, stageErr(name, "a collection path, such as /users or /users/alice/orders, is required.")
		}
		parent := p.db + "/documents/" + path
		docs, err := p.all()
		if err != nil {
			return nil, err
		}
		var out []row
		for _, d := range docs {
			if i := strings.LastIndex(d.Name, "/"); i >= 0 && d.Name[:i] == parent {
				out = append(out, docRow(d))
			}
		}
		return out, nil
	case "collection_group":
		id := arg(1).GetStringValue()
		if id == "" {
			return nil, stageErr(name, "a collection ID is required.")
		}
		under := p.db + "/documents/"
		if anc := strings.Trim(arg(0).GetReferenceValue(), "/"); anc != "" {
			under += anc + "/"
		}
		docs, err := p.all()
		if err != nil {
			return nil, err
		}
		var out []row
		for _, d := range docs {
			if collectionOf(d.Name) == id && strings.HasPrefix(d.Name, under) {
				out = append(out, docRow(d))
			}
		}
		return out, nil
	case "database":
		docs, err := p.all()
		if err != nil {
			return nil, err
		}
		out := make([]row, 0, len(docs))
		for _, d := range docs {
			out = append(out, docRow(d))
		}
		return out, nil
	case "documents":
		docs, err := p.all()
		if err != nil {
			return nil, err
		}
		byName := make(map[string]*firestorepb.Document, len(docs))
		for _, d := range docs {
			byName[d.Name] = d
		}
		var out []row
		seen := map[string]bool{}
		for _, a := range args {
			n := p.db + "/documents/" + strings.Trim(a.GetReferenceValue(), "/")
			if d, ok := byName[n]; ok && !seen[n] {
				seen[n] = true
				out = append(out, docRow(d))
			}
		}
		return out, nil
	case "where":
		var out []row
		for _, r := range rows {
			v, err := p.eval(arg(0), r)
			if err != nil {
				if isEvalError(err) {
					continue
				}
				return nil, err
			}
			if v.GetBooleanValue() {
				out = append(out, r)
			}
		}
		return out, nil
	case "select", "add_fields":
		fields := arg(0).GetMapValue().GetFields()
		if len(fields) == 0 {
			return nil, stageErr(name, "at least one field or expression is required.")
		}
		out := make([]row, 0, len(rows))
		for _, r := range rows {
			var n row
			if name == "select" {
				n = row{fields: map[string]*firestorepb.Value{}}
			} else {
				n = r.clone()
			}
			for _, alias := range sortedKeys(fields) {
				v, err := p.evalStrict(fields[alias], r)
				if err != nil {
					return nil, err
				}
				if alias == "__name__" && v.GetReferenceValue() != "" {
					n.name = v.GetReferenceValue()
					continue
				}
				if v != nil {
					setPath(n.fields, splitPath(alias), v)
				} else if name == "add_fields" {
					deletePath(n.fields, splitPath(alias))
				}
			}
			out = append(out, n)
		}
		return out, nil
	case "remove_fields":
		out := make([]row, 0, len(rows))
		for _, r := range rows {
			n := r.clone()
			n.fields = cloneFields(r.fields)
			for _, a := range args {
				f := a.GetFieldReferenceValue()
				if f == "" {
					return nil, stageErr(name, "each argument is a field.")
				}
				deletePath(n.fields, splitPath(f))
			}
			out = append(out, n)
		}
		return out, nil
	case "replace_with":
		out := make([]row, 0, len(rows))
		for _, r := range rows {
			v, err := p.evalStrict(arg(0), r)
			if err != nil {
				return nil, err
			}
			m := v.GetMapValue()
			if m == nil {
				return nil, stageErr(name, "the expression must evaluate to a map, not %s.", typeName(v))
			}
			n := r
			n.fields = m.GetFields()
			out = append(out, n)
		}
		return out, nil
	case "sort":
		if len(args) == 0 {
			return nil, stageErr(name, "at least one ordering is required.")
		}
		type key struct {
			vals []*firestorepb.Value
		}
		keys := make([]key, len(rows))
		for i, r := range rows {
			for _, o := range args {
				v, err := p.eval(o.GetMapValue().GetFields()["expression"], r)
				if err != nil && !isEvalError(err) {
					return nil, err
				}
				keys[i].vals = append(keys[i].vals, v)
			}
		}
		idx := make([]int, len(rows))
		for i := range idx {
			idx[i] = i
		}
		var bad error
		sort.SliceStable(idx, func(x, y int) bool {
			for j, o := range args {
				dir := o.GetMapValue().GetFields()["direction"].GetStringValue()
				c := compareValues(keys[idx[x]].vals[j], keys[idx[y]].vals[j])
				if dir == "descending" {
					c = -c
				} else if dir != "ascending" && bad == nil {
					bad = stageErr(name, "direction %q is invalid: want ascending or descending.", dir)
				}
				if c != 0 {
					return c < 0
				}
			}
			return false
		})
		if bad != nil {
			return nil, bad
		}
		out := make([]row, len(rows))
		for i, j := range idx {
			out[i] = rows[j]
		}
		return out, nil
	case "limit", "offset":
		n, ok := asInt(arg(0))
		if !ok || n < 0 {
			return nil, stageErr(name, "a non-negative integer is required.")
		}
		if name == "limit" {
			return rows[:min(int(n), len(rows))], nil
		}
		return rows[min(int(n), len(rows)):], nil
	case "aggregate":
		return p.aggregate(arg(0).GetMapValue().GetFields(), arg(1).GetMapValue().GetFields(), rows)
	case "distinct":
		fields := arg(0).GetMapValue().GetFields()
		if len(fields) == 0 {
			return nil, stageErr(name, "at least one field or expression is required.")
		}
		return p.aggregate(nil, fields, rows)
	case "unnest":
		alias := arg(1).GetFieldReferenceValue()
		if alias == "" {
			return nil, stageErr(name, "an alias is required.")
		}
		index := opts["index_field"].GetFieldReferenceValue()
		var out []row
		for _, r := range rows {
			v, err := p.evalStrict(arg(0), r)
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			arr := v.GetArrayValue()
			if arr == nil {
				// A value that is not an array is passed through as
				// itself, under the alias.
				n := r.clone()
				n.fields = cloneFields(r.fields)
				setPath(n.fields, splitPath(alias), v)
				out = append(out, n)
				continue
			}
			for i, el := range arr.GetValues() {
				n := r.clone()
				n.fields = cloneFields(r.fields)
				setPath(n.fields, splitPath(alias), el)
				if index != "" {
					setPath(n.fields, splitPath(index), intValue(int64(i)))
				}
				out = append(out, n)
			}
		}
		return out, nil
	case "sample":
		mode := arg(1).GetStringValue()
		switch mode {
		case "documents", "":
			n, ok := asInt(arg(0))
			if !ok || n < 0 {
				return nil, stageErr(name, "a non-negative number of documents is required.")
			}
			out := append([]row(nil), rows...)
			rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
			return out[:min(int(n), len(out))], nil
		case "percent":
			f, ok := asFloat(arg(0))
			if !ok || f < 0 || f > 1 {
				return nil, stageErr(name, "a percentage between 0 and 1 is required.")
			}
			var out []row
			for _, r := range rows {
				if rand.Float64() < f {
					out = append(out, r)
				}
			}
			return out, nil
		default:
			return nil, stageErr(name, "mode %q is invalid: want documents or percent.", mode)
		}
	case "union":
		other := arg(0).GetPipelineValue()
		if other == nil {
			return nil, stageErr(name, "a pipeline is required.")
		}
		more, err := p.run(other)
		if err != nil {
			return nil, err
		}
		return append(append([]row(nil), rows...), more...), nil
	case "find_nearest":
		return p.findNearest(args, opts, rows)
	}
	return nil, status.Errorf(codes.Internal, "the stage %q is listed as served but has no evaluation", name)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// aggregate groups rows by the groups expressions and computes the
// accumulators over each group; with no groups, it is one row over all.
func (p *pipelineRun) aggregate(accs, groups map[string]*firestorepb.Value, rows []row) ([]row, error) {
	for alias, a := range accs {
		fn := a.GetFunctionValue()
		if fn == nil {
			return nil, stageErr("aggregate", "%q is not an aggregate function.", alias)
		}
		if served, known := pipelineAggregates[fn.GetName()]; !known {
			return nil, status.Errorf(codes.InvalidArgument, "Unknown aggregate function %q.", fn.GetName())
		} else if !served {
			return nil, status.Errorf(codes.Unimplemented, "the aggregate function %q is not served here", fn.GetName())
		}
	}
	type group struct {
		key  []*firestorepb.Value
		rows []row
	}
	var order []*group
	byKey := map[string]*group{}
	gk := sortedKeys(groups)
	for _, r := range rows {
		var key []*firestorepb.Value
		for _, g := range gk {
			v, err := p.evalStrict(groups[g], r)
			if err != nil {
				return nil, err
			}
			key = append(key, v)
		}
		k := valuesKey(key)
		grp, ok := byKey[k]
		if !ok {
			grp = &group{key: key}
			byKey[k] = grp
			order = append(order, grp)
		}
		grp.rows = append(grp.rows, r)
	}
	if len(groups) == 0 && len(order) == 0 {
		order = append(order, &group{})
	}
	out := make([]row, 0, len(order))
	for _, grp := range order {
		n := row{fields: map[string]*firestorepb.Value{}}
		for i, g := range gk {
			if grp.key[i] != nil {
				setPath(n.fields, splitPath(g), grp.key[i])
			} else {
				setPath(n.fields, splitPath(g), nullValue())
			}
		}
		for _, alias := range sortedKeys(accs) {
			v, err := p.accumulate(accs[alias].GetFunctionValue(), grp.rows)
			if err != nil {
				return nil, err
			}
			setPath(n.fields, splitPath(alias), v)
		}
		out = append(out, n)
	}
	return out, nil
}

// pipelineAggregates are the aggregate functions Google documents, and
// whether each is served here.
var pipelineAggregates = map[string]bool{
	"count": true, "count_if": true, "count_distinct": true, "sum": true, "average": true,
	"minimum": true, "maximum": true, "first": true, "last": true, "array_agg": true, "array_agg_distinct": true,
	"minimum_n": false, "maximum_n": false,
}

func (p *pipelineRun) accumulate(fn *firestorepb.Function, rows []row) (*firestorepb.Value, error) {
	name := fn.GetName()
	var vals []*firestorepb.Value
	if len(fn.GetArgs()) > 0 {
		for _, r := range rows {
			v, err := p.eval(fn.GetArgs()[0], r)
			if err != nil {
				if isEvalError(err) {
					continue
				}
				return nil, err
			}
			vals = append(vals, v)
		}
	}
	present := func() []*firestorepb.Value {
		var out []*firestorepb.Value
		for _, v := range vals {
			if v != nil && !isNull(v) {
				out = append(out, v)
			}
		}
		return out
	}
	switch name {
	case "count":
		if len(fn.GetArgs()) == 0 {
			return intValue(int64(len(rows))), nil
		}
		return intValue(int64(len(present()))), nil
	case "count_if":
		n := 0
		for _, v := range vals {
			if v.GetBooleanValue() {
				n++
			}
		}
		return intValue(int64(n)), nil
	case "count_distinct":
		seen := map[string]bool{}
		for _, v := range present() {
			seen[valuesKey([]*firestorepb.Value{v})] = true
		}
		return intValue(int64(len(seen))), nil
	case "sum", "average":
		var isum int64
		var fsum float64
		allInt, n := true, 0
		for _, v := range present() {
			switch x := v.GetValueType().(type) {
			case *firestorepb.Value_IntegerValue:
				if allInt {
					s := isum + x.IntegerValue
					if (s > isum) == (x.IntegerValue > 0) || x.IntegerValue == 0 {
						isum = s
					} else {
						allInt, fsum = false, float64(isum)+float64(x.IntegerValue)
					}
				} else {
					fsum += float64(x.IntegerValue)
				}
				n++
			case *firestorepb.Value_DoubleValue:
				if allInt {
					allInt, fsum = false, float64(isum)
				}
				fsum += x.DoubleValue
				n++
			}
		}
		if name == "average" {
			if n == 0 {
				return nullValue(), nil
			}
			if allInt {
				fsum = float64(isum)
			}
			return doubleValue(fsum / float64(n)), nil
		}
		if allInt {
			return intValue(isum), nil
		}
		return doubleValue(fsum), nil
	case "minimum", "maximum":
		var best *firestorepb.Value
		for _, v := range present() {
			if best == nil || (name == "minimum" && compareValues(v, best) < 0) || (name == "maximum" && compareValues(v, best) > 0) {
				best = v
			}
		}
		if best == nil {
			return nullValue(), nil
		}
		return best, nil
	case "first", "last":
		if len(vals) == 0 {
			return nullValue(), nil
		}
		v := vals[0]
		if name == "last" {
			v = vals[len(vals)-1]
		}
		if v == nil {
			return nullValue(), nil
		}
		return v, nil
	case "array_agg", "array_agg_distinct":
		out := []*firestorepb.Value{}
		seen := map[string]bool{}
		for _, v := range vals {
			if v == nil {
				v = nullValue()
			}
			if name == "array_agg_distinct" {
				k := valuesKey([]*firestorepb.Value{v})
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			out = append(out, v)
		}
		return arrayValue(out), nil
	}
	return nil, status.Errorf(codes.Unimplemented, "the aggregate function %q is not served here", name)
}

func (p *pipelineRun) findNearest(args []*firestorepb.Value, opts map[string]*firestorepb.Value, rows []row) ([]row, error) {
	if len(args) < 3 {
		return nil, stageErr("find_nearest", "a vector field, a query vector and a distance measure are required.")
	}
	query, ok := vectorOf(args[1])
	if !ok {
		return nil, stageErr("find_nearest", "the query vector must be a vector.")
	}
	measure := args[2].GetStringValue()
	if measure != "euclidean" && measure != "cosine" && measure != "dot_product" {
		return nil, stageErr("find_nearest", "distance measure %q is invalid: want euclidean, cosine or dot_product.", measure)
	}
	type scored struct {
		r row
		d float64
	}
	var out []scored
	for _, r := range rows {
		v, err := p.eval(args[0], r)
		if err != nil && !isEvalError(err) {
			return nil, err
		}
		vec, ok := vectorOf(v)
		if !ok || len(vec) != len(query) {
			continue
		}
		d, ok := distance(measure, vec, query)
		if !ok {
			continue
		}
		out = append(out, scored{r, d})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if measure == "dot_product" {
			return out[i].d > out[j].d
		}
		return out[i].d < out[j].d
	})
	if l, ok := asInt(opts["limit"]); ok && l >= 0 && int(l) < len(out) {
		out = out[:l]
	}
	df := opts["distance_field"].GetFieldReferenceValue()
	if df == "" {
		df = opts["distance_field"].GetStringValue()
	}
	res := make([]row, 0, len(out))
	for _, s := range out {
		r := s.r
		if df != "" {
			r = r.clone()
			r.fields = cloneFields(s.r.fields)
			setPath(r.fields, splitPath(df), doubleValue(s.d))
		}
		res = append(res, r)
	}
	return res, nil
}

func distance(measure string, a, b []float64) (float64, bool) {
	switch measure {
	case "euclidean":
		var s float64
		for i := range a {
			s += (a[i] - b[i]) * (a[i] - b[i])
		}
		return math.Sqrt(s), true
	case "dot_product":
		var s float64
		for i := range a {
			s += a[i] * b[i]
		}
		return s, true
	default:
		var dot, na, nb float64
		for i := range a {
			dot += a[i] * b[i]
			na += a[i] * a[i]
			nb += b[i] * b[i]
		}
		if na == 0 || nb == 0 {
			return 0, false
		}
		return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb)), true
	}
}

// vectorOf is v's elements, when v is a vector (a map with
// __type__: "__vector__") or an array of numbers.
func vectorOf(v *firestorepb.Value) ([]float64, bool) {
	var arr *firestorepb.ArrayValue
	if m := v.GetMapValue(); m != nil {
		if m.GetFields()["__type__"].GetStringValue() != "__vector__" {
			return nil, false
		}
		arr = m.GetFields()["value"].GetArrayValue()
	} else {
		arr = v.GetArrayValue()
	}
	if arr == nil {
		return nil, false
	}
	out := make([]float64, 0, len(arr.GetValues()))
	for _, e := range arr.GetValues() {
		f, ok := asFloat(e)
		if !ok {
			return nil, false
		}
		out = append(out, f)
	}
	return out, true
}

// --- explain ---

func (p *pipelineRun) explainText(pl *firestorepb.Pipeline, mode, format string, results int, took time.Duration) string {
	var stages []string
	for _, s := range pl.GetStages() {
		stages = append(stages, s.GetName())
	}
	if format == "json" {
		m := map[string]any{"plan": map[string]any{"stages": stages, "index_mode": "none: evaluated by CloudBurrow without an index"}}
		if mode == "analyze" {
			m["execution"] = map[string]any{"results_returned": results, "execution_duration": took.String(), "stage_outputs": p.stats}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	var b strings.Builder
	b.WriteString("Plan:\n")
	for i, s := range stages {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	b.WriteString("  (evaluated by CloudBurrow over the emulator's documents, without an index)\n")
	if mode == "analyze" {
		b.WriteString("Execution:\n")
		fmt.Fprintf(&b, "  results returned: %d\n", results)
		fmt.Fprintf(&b, "  execution duration: %s\n", took)
		for _, s := range p.stats {
			fmt.Fprintf(&b, "  documents out of %s\n", s)
		}
	}
	return b.String()
}

// --- expressions ---

// evalError is an expression that evaluated to an error (a type error,
// say): a where stage drops the row, is_error and if_error catch it, and
// anywhere else it fails the pipeline INVALID_ARGUMENT.
type evalError struct{ msg string }

func (e *evalError) Error() string { return e.msg }

func isEvalError(err error) bool {
	_, ok := err.(*evalError)
	return ok
}

func evalErrf(format string, args ...any) error { return &evalError{fmt.Sprintf(format, args...)} }

// evalStrict is eval, with an expression error failing the pipeline.
func (p *pipelineRun) evalStrict(v *firestorepb.Value, r row) (*firestorepb.Value, error) {
	out, err := p.eval(v, r)
	if err != nil && isEvalError(err) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return out, err
}

// pipelineFunctions are the expression functions served here. Every other
// name is refused: UNIMPLEMENTED if Google documents it (pipelineUnserved),
// INVALID_ARGUMENT otherwise.
var pipelineFunctions = map[string]func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error){}

// pipelineUnserved are the functions Google's reference documents that are
// not evaluated here.
var pipelineUnserved = map[string]string{
	"score":              "full-text search scores need a search index",
	"document_matches":   "full-text search needs a search index",
	"geo_distance":       "geographic distance is not evaluated here",
	"storage_size":       "the emulator does not report a document's storage size",
	"array_filter":       "lambda expressions are not evaluated here",
	"array_transform":    "lambda expressions are not evaluated here",
	"switch_on":          "switch_on is not evaluated here",
	"rand":               "rand is not evaluated here",
	"current_document":   "current_document is not evaluated here",
	"timestamp_trunc":    "timestamp_trunc is not evaluated here",
	"timestamp_diff":     "timestamp_diff is not evaluated here",
	"timestamp_extract":  "timestamp_extract is not evaluated here",
	"regex_find":         "regex_find is not evaluated here",
	"regex_find_all":     "regex_find_all is not evaluated here",
	"reference_slice":    "reference_slice is not evaluated here",
	"minimum_n":          "minimum_n is not evaluated here",
	"maximum_n":          "maximum_n is not evaluated here",
	"array_first_n":      "array_first_n is not evaluated here",
	"array_last_n":       "array_last_n is not evaluated here",
	"array_index_of_all": "array_index_of_all is not evaluated here",
	"map_entries":        "map_entries is not evaluated here",
	"offset":             "offset is not evaluated here",
	"cmp":                "cmp is not evaluated here",
}

func (p *pipelineRun) eval(v *firestorepb.Value, r row) (*firestorepb.Value, error) {
	switch x := v.GetValueType().(type) {
	case nil:
		return nil, status.Error(codes.InvalidArgument, "an expression is missing.")
	case *firestorepb.Value_FieldReferenceValue:
		return r.field(x.FieldReferenceValue), nil
	case *firestorepb.Value_FunctionValue:
		name := x.FunctionValue.GetName()
		f, ok := pipelineFunctions[name]
		if !ok {
			if why, ok := pipelineUnserved[name]; ok {
				return nil, status.Errorf(codes.Unimplemented, "the pipeline function %q is not served here: %s", name, why)
			}
			if _, ok := pipelineAggregates[name]; ok {
				return nil, status.Errorf(codes.InvalidArgument, "the aggregate function %q is allowed only in an aggregate stage.", name)
			}
			return nil, status.Errorf(codes.InvalidArgument, "Unknown pipeline function %q.", name)
		}
		return f(p, x.FunctionValue.GetArgs(), r)
	case *firestorepb.Value_PipelineValue:
		return nil, status.Error(codes.InvalidArgument, "a pipeline is not an expression.")
	case *firestorepb.Value_ArrayValue:
		out := make([]*firestorepb.Value, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			ev, err := p.eval(e, r)
			if err != nil {
				return nil, err
			}
			if ev == nil {
				ev = nullValue()
			}
			out = append(out, ev)
		}
		return arrayValue(out), nil
	case *firestorepb.Value_MapValue:
		if x.MapValue.GetFields()["__type__"] != nil {
			return v, nil
		}
		out := make(map[string]*firestorepb.Value, len(x.MapValue.GetFields()))
		for k, e := range x.MapValue.GetFields() {
			ev, err := p.eval(e, r)
			if err != nil {
				return nil, err
			}
			if ev != nil {
				out[k] = ev
			}
		}
		return mapValue(out), nil
	}
	return v, nil
}

// field is the value at path in r, or nil when it is absent.
func (r row) field(path string) *firestorepb.Value {
	switch path {
	case "__name__":
		if r.name == "" {
			return nil
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: r.name}}
	case "__create_time__":
		if r.create == nil {
			return nil
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_TimestampValue{TimestampValue: r.create}}
	case "__update_time__":
		if r.update == nil {
			return nil
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_TimestampValue{TimestampValue: r.update}}
	}
	fields := r.fields
	parts := splitPath(path)
	for i, p := range parts {
		v, ok := fields[p]
		if !ok {
			return nil
		}
		if i == len(parts)-1 {
			return v
		}
		m := v.GetMapValue()
		if m == nil {
			return nil
		}
		fields = m.GetFields()
	}
	return nil
}

// splitPath splits a field path on its dots, a backquoted segment
// (`a.b`) whole.
func splitPath(path string) []string {
	var parts []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '\\' && quoted && i+1 < len(path):
			i++
			cur.WriteByte(path[i])
		case c == '`':
			quoted = !quoted
		case c == '.' && !quoted:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, cur.String())
}

func cloneFields(m map[string]*firestorepb.Value) map[string]*firestorepb.Value {
	out := make(map[string]*firestorepb.Value, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// setPath sets the value at parts in fields, copying every map on the
// way, so the documents read stay as read.
func setPath(fields map[string]*firestorepb.Value, parts []string, v *firestorepb.Value) {
	if len(parts) == 1 {
		fields[parts[0]] = v
		return
	}
	var inner map[string]*firestorepb.Value
	if m := fields[parts[0]].GetMapValue(); m != nil {
		inner = cloneFields(m.GetFields())
	} else {
		inner = map[string]*firestorepb.Value{}
	}
	setPath(inner, parts[1:], v)
	fields[parts[0]] = mapValue(inner)
}

func deletePath(fields map[string]*firestorepb.Value, parts []string) {
	if len(parts) == 1 {
		delete(fields, parts[0])
		return
	}
	m := fields[parts[0]].GetMapValue()
	if m == nil {
		return
	}
	inner := cloneFields(m.GetFields())
	deletePath(inner, parts[1:])
	fields[parts[0]] = mapValue(inner)
}

// --- values ---

func nullValue() *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}
}
func boolValue(b bool) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_BooleanValue{BooleanValue: b}}
}
func intValue(n int64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: n}}
}
func doubleValue(f float64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: f}}
}
func stringValue(s string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
}
func arrayValue(vs []*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vs}}}
}
func mapValue(m map[string]*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: m}}}
}
func timestampValue(t time.Time) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_TimestampValue{TimestampValue: timestamppb.New(t)}}
}

func isNull(v *firestorepb.Value) bool {
	_, ok := v.GetValueType().(*firestorepb.Value_NullValue)
	return ok
}

func asInt(v *firestorepb.Value) (int64, bool) {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_IntegerValue:
		return x.IntegerValue, true
	case *firestorepb.Value_DoubleValue:
		if x.DoubleValue == math.Trunc(x.DoubleValue) {
			return int64(x.DoubleValue), true
		}
	}
	return 0, false
}

func asFloat(v *firestorepb.Value) (float64, bool) {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_IntegerValue:
		return float64(x.IntegerValue), true
	case *firestorepb.Value_DoubleValue:
		return x.DoubleValue, true
	}
	return 0, false
}

func isNumber(v *firestorepb.Value) bool {
	_, ok := asFloat(v)
	return ok
}

func isVector(v *firestorepb.Value) bool {
	return v.GetMapValue().GetFields()["__type__"].GetStringValue() == "__vector__"
}

// typeRank is v's place in Firestore's order of value types.
func typeRank(v *firestorepb.Value) int {
	switch x := v.GetValueType().(type) {
	case nil:
		return -1 // absent sorts first
	case *firestorepb.Value_NullValue:
		return 0
	case *firestorepb.Value_BooleanValue:
		return 1
	case *firestorepb.Value_IntegerValue:
		return 3
	case *firestorepb.Value_DoubleValue:
		if math.IsNaN(x.DoubleValue) {
			return 2
		}
		return 3
	case *firestorepb.Value_TimestampValue:
		return 4
	case *firestorepb.Value_StringValue:
		return 5
	case *firestorepb.Value_BytesValue:
		return 6
	case *firestorepb.Value_ReferenceValue:
		return 7
	case *firestorepb.Value_GeoPointValue:
		return 8
	case *firestorepb.Value_ArrayValue:
		return 9
	case *firestorepb.Value_MapValue:
		if isVector(v) {
			return 10
		}
		return 11
	}
	return 12
}

func typeName(v *firestorepb.Value) string {
	switch v.GetValueType().(type) {
	case nil:
		return "absent"
	case *firestorepb.Value_NullValue:
		return "null"
	case *firestorepb.Value_BooleanValue:
		return "boolean"
	case *firestorepb.Value_IntegerValue:
		return "int64"
	case *firestorepb.Value_DoubleValue:
		return "float64"
	case *firestorepb.Value_TimestampValue:
		return "timestamp"
	case *firestorepb.Value_StringValue:
		return "string"
	case *firestorepb.Value_BytesValue:
		return "bytes"
	case *firestorepb.Value_ReferenceValue:
		return "reference"
	case *firestorepb.Value_GeoPointValue:
		return "geo_point"
	case *firestorepb.Value_ArrayValue:
		return "array"
	case *firestorepb.Value_MapValue:
		if isVector(v) {
			return "vector"
		}
		return "map"
	}
	return "unknown"
}

func cmpInt[T int | int64 | float64 | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareValues orders a and b as Firestore orders values.
func compareValues(a, b *firestorepb.Value) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return cmpInt(ra, rb)
	}
	switch x := a.GetValueType().(type) {
	case *firestorepb.Value_BooleanValue:
		ab, bb := 0, 0
		if x.BooleanValue {
			ab = 1
		}
		if b.GetBooleanValue() {
			bb = 1
		}
		return cmpInt(ab, bb)
	case *firestorepb.Value_IntegerValue, *firestorepb.Value_DoubleValue:
		ai, aInt := a.GetValueType().(*firestorepb.Value_IntegerValue)
		bi, bInt := b.GetValueType().(*firestorepb.Value_IntegerValue)
		if aInt && bInt {
			return cmpInt(ai.IntegerValue, bi.IntegerValue)
		}
		fa, _ := asFloat(a)
		fb, _ := asFloat(b)
		if math.IsNaN(fa) || math.IsNaN(fb) {
			return 0
		}
		return cmpInt(fa, fb)
	case *firestorepb.Value_TimestampValue:
		ta, tb := x.TimestampValue.AsTime(), b.GetTimestampValue().AsTime()
		return ta.Compare(tb)
	case *firestorepb.Value_StringValue:
		return cmpInt(x.StringValue, b.GetStringValue())
	case *firestorepb.Value_BytesValue:
		return cmpInt(string(x.BytesValue), string(b.GetBytesValue()))
	case *firestorepb.Value_ReferenceValue:
		as, bs := strings.Split(x.ReferenceValue, "/"), strings.Split(b.GetReferenceValue(), "/")
		for i := 0; i < len(as) && i < len(bs); i++ {
			if c := cmpInt(as[i], bs[i]); c != 0 {
				return c
			}
		}
		return cmpInt(len(as), len(bs))
	case *firestorepb.Value_GeoPointValue:
		if c := cmpInt(x.GeoPointValue.GetLatitude(), b.GetGeoPointValue().GetLatitude()); c != 0 {
			return c
		}
		return cmpInt(x.GeoPointValue.GetLongitude(), b.GetGeoPointValue().GetLongitude())
	case *firestorepb.Value_ArrayValue:
		av, bv := x.ArrayValue.GetValues(), b.GetArrayValue().GetValues()
		for i := 0; i < len(av) && i < len(bv); i++ {
			if c := compareValues(av[i], bv[i]); c != 0 {
				return c
			}
		}
		return cmpInt(len(av), len(bv))
	case *firestorepb.Value_MapValue:
		if isVector(a) {
			va, _ := vectorOf(a)
			vb, _ := vectorOf(b)
			if c := cmpInt(len(va), len(vb)); c != 0 {
				return c
			}
			for i := range va {
				if c := cmpInt(va[i], vb[i]); c != 0 {
					return c
				}
			}
			return 0
		}
		am, bm := x.MapValue.GetFields(), b.GetMapValue().GetFields()
		ak, bk := sortedKeys(am), sortedKeys(bm)
		for i := 0; i < len(ak) && i < len(bk); i++ {
			if c := cmpInt(ak[i], bk[i]); c != 0 {
				return c
			}
			if c := compareValues(am[ak[i]], bm[bk[i]]); c != 0 {
				return c
			}
		}
		return cmpInt(len(ak), len(bk))
	}
	return 0
}

// equalValues is Firestore's equality: numbers by value, across int64
// and float64; NaN equals nothing.
func equalValues(a, b *firestorepb.Value) bool {
	if a == nil || b == nil {
		return false
	}
	if f, ok := asFloat(a); ok && math.IsNaN(f) {
		return false
	}
	if isNumber(a) != isNumber(b) || (!isNumber(a) && typeRank(a) != typeRank(b)) {
		return false
	}
	return compareValues(a, b) == 0
}

// valuesKey is a key equal values share, for grouping and distinct.
func valuesKey(vs []*firestorepb.Value) string {
	var b strings.Builder
	for _, v := range vs {
		if v == nil {
			b.WriteString("\x00absent")
		} else {
			c := v
			if i, ok := v.GetValueType().(*firestorepb.Value_IntegerValue); ok {
				c = doubleValue(float64(i.IntegerValue))
			}
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
			b.Write(raw)
		}
		b.WriteString("\x00|")
	}
	return b.String()
}

// --- the functions ---

func init() {
	f := pipelineFunctions
	type fn = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error)
	// evalArgs evaluates every argument, n of them when n >= 0.
	evalArgs := func(name string, n int, p *pipelineRun, args []*firestorepb.Value, r row) ([]*firestorepb.Value, error) {
		if n >= 0 && len(args) != n {
			return nil, status.Errorf(codes.InvalidArgument, "the function %q takes %d arguments, not %d.", name, n, len(args))
		}
		out := make([]*firestorepb.Value, len(args))
		for i, a := range args {
			v, err := p.eval(a, r)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	simple := func(name string, n int, do func(vs []*firestorepb.Value) (*firestorepb.Value, error)) {
		f[name] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
			vs, err := evalArgs(name, n, p, args, r)
			if err != nil {
				return nil, err
			}
			return do(vs)
		}
	}

	// Logic.
	logic := func(name string, and bool) fn {
		return func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
			for _, a := range args {
				v, err := p.eval(a, r)
				if err != nil {
					return nil, err
				}
				b, ok := v.GetValueType().(*firestorepb.Value_BooleanValue)
				if !ok {
					return nil, evalErrf("%s: %s is not a boolean", name, typeName(v))
				}
				if b.BooleanValue != and {
					return boolValue(!and), nil
				}
			}
			return boolValue(and), nil
		}
	}
	f["and"], f["or"] = logic("and", true), logic("or", false)
	simple("xor", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		out := false
		for _, v := range vs {
			b, ok := v.GetValueType().(*firestorepb.Value_BooleanValue)
			if !ok {
				return nil, evalErrf("xor: %s is not a boolean", typeName(v))
			}
			out = out != b.BooleanValue
		}
		return boolValue(out), nil
	})
	f["nor"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		v, err := f["or"](p, args, r)
		if err != nil {
			return nil, err
		}
		return boolValue(!v.GetBooleanValue()), nil
	}
	simple("not", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		b, ok := vs[0].GetValueType().(*firestorepb.Value_BooleanValue)
		if !ok {
			return nil, evalErrf("not: %s is not a boolean", typeName(vs[0]))
		}
		return boolValue(!b.BooleanValue), nil
	})
	f["conditional"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 3 {
			return nil, status.Error(codes.InvalidArgument, "the function \"conditional\" takes 3 arguments.")
		}
		c, err := p.eval(args[0], r)
		if err != nil {
			return nil, err
		}
		if c.GetBooleanValue() {
			return p.eval(args[1], r)
		}
		return p.eval(args[2], r)
	}

	// Comparison.
	cmp := func(name string, ok func(int) bool) {
		simple(name, 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			a, b := vs[0], vs[1]
			if a == nil || b == nil {
				return boolValue(false), nil
			}
			if isNumber(a) && isNumber(b) {
				fa, _ := asFloat(a)
				fb, _ := asFloat(b)
				if math.IsNaN(fa) || math.IsNaN(fb) {
					return boolValue(false), nil
				}
			} else if typeRank(a) != typeRank(b) {
				return boolValue(false), nil
			}
			return boolValue(ok(compareValues(a, b))), nil
		})
	}
	cmp("less_than", func(c int) bool { return c < 0 })
	cmp("less_than_or_equal", func(c int) bool { return c <= 0 })
	cmp("greater_than", func(c int) bool { return c > 0 })
	cmp("greater_than_or_equal", func(c int) bool { return c >= 0 })
	simple("equal", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		return boolValue(equalValues(vs[0], vs[1])), nil
	})
	simple("not_equal", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil || vs[1] == nil {
			return boolValue(vs[0] != nil || vs[1] != nil), nil
		}
		return boolValue(!equalValues(vs[0], vs[1])), nil
	})
	anyOf := func(name string, want bool) {
		simple(name, 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			arr := vs[1].GetArrayValue()
			if arr == nil {
				return nil, evalErrf("%s: the values are %s, not an array", name, typeName(vs[1]))
			}
			if vs[0] == nil {
				return boolValue(!want), nil
			}
			for _, e := range arr.GetValues() {
				if equalValues(vs[0], e) {
					return boolValue(want), nil
				}
			}
			return boolValue(!want), nil
		})
	}
	anyOf("equal_any", true)
	anyOf("not_equal_any", false)

	// Existence and types.
	simple("exists", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) { return boolValue(vs[0] != nil), nil })
	simple("is_absent", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) { return boolValue(vs[0] == nil), nil })
	simple("is_null", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) { return boolValue(isNull(vs[0])), nil })
	simple("is_nan", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		x, ok := vs[0].GetValueType().(*firestorepb.Value_DoubleValue)
		return boolValue(ok && math.IsNaN(x.DoubleValue)), nil
	})
	f["is_error"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 1 {
			return nil, status.Error(codes.InvalidArgument, "the function \"is_error\" takes 1 argument.")
		}
		_, err := p.eval(args[0], r)
		if err != nil && !isEvalError(err) {
			return nil, err
		}
		return boolValue(err != nil), nil
	}
	f["if_error"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 2 {
			return nil, status.Error(codes.InvalidArgument, "the function \"if_error\" takes 2 arguments.")
		}
		v, err := p.eval(args[0], r)
		if err != nil {
			if !isEvalError(err) {
				return nil, err
			}
			return p.eval(args[1], r)
		}
		return v, nil
	}
	f["if_absent"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 2 {
			return nil, status.Error(codes.InvalidArgument, "the function \"if_absent\" takes 2 arguments.")
		}
		v, err := p.eval(args[0], r)
		if err != nil || v != nil {
			return v, err
		}
		return p.eval(args[1], r)
	}
	f["if_null"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 2 {
			return nil, status.Error(codes.InvalidArgument, "the function \"if_null\" takes 2 arguments.")
		}
		v, err := p.eval(args[0], r)
		if err != nil || (v != nil && !isNull(v)) {
			return v, err
		}
		return p.eval(args[1], r)
	}
	f["coalesce"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		for _, a := range args {
			v, err := p.eval(a, r)
			if err != nil {
				return nil, err
			}
			if v != nil && !isNull(v) {
				return v, nil
			}
		}
		return nullValue(), nil
	}
	simple("type", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) { return stringValue(typeName(vs[0])), nil })
	simple("is_type", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		want := vs[1].GetStringValue()
		got := typeName(vs[0])
		return boolValue(got == want || (want == "number" && isNumber(vs[0]))), nil
	})

	// Arithmetic.
	arith := func(name string, ints func(a, b int64) (int64, bool), floats func(a, b float64) float64) {
		simple(name, 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			a, b := vs[0], vs[1]
			if a == nil || b == nil || isNull(a) || isNull(b) {
				return nullValue(), nil
			}
			ai, aInt := a.GetValueType().(*firestorepb.Value_IntegerValue)
			bi, bInt := b.GetValueType().(*firestorepb.Value_IntegerValue)
			if aInt && bInt && ints != nil {
				n, ok := ints(ai.IntegerValue, bi.IntegerValue)
				if !ok {
					return nil, evalErrf("%s: integer overflow or division by zero", name)
				}
				return intValue(n), nil
			}
			fa, ok1 := asFloat(a)
			fb, ok2 := asFloat(b)
			if !ok1 || !ok2 {
				return nil, evalErrf("%s: %s and %s are not both numbers", name, typeName(a), typeName(b))
			}
			return doubleValue(floats(fa, fb)), nil
		})
	}
	arith("add", func(a, b int64) (int64, bool) { s := a + b; return s, (s > a) == (b > 0) || b == 0 },
		func(a, b float64) float64 { return a + b })
	arith("subtract", func(a, b int64) (int64, bool) { s := a - b; return s, (s < a) == (b > 0) || b == 0 },
		func(a, b float64) float64 { return a - b })
	arith("multiply", func(a, b int64) (int64, bool) {
		if a == 0 || b == 0 {
			return 0, true
		}
		s := a * b
		return s, s/b == a && !(a == -1 && b == math.MinInt64) && !(b == -1 && a == math.MinInt64)
	}, func(a, b float64) float64 { return a * b })
	arith("divide", func(a, b int64) (int64, bool) {
		if b == 0 || (a == math.MinInt64 && b == -1) {
			return 0, false
		}
		return a / b, true
	}, func(a, b float64) float64 { return a / b })
	arith("mod", func(a, b int64) (int64, bool) {
		if b == 0 {
			return 0, false
		}
		return a % b, true
	}, math.Mod)
	arith("pow", nil, math.Pow)
	arith("log", nil, func(a, b float64) float64 { return math.Log(a) / math.Log(b) })
	unary := func(name string, keepInt bool, do func(float64) float64) {
		simple(name, 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil || isNull(vs[0]) {
				return nullValue(), nil
			}
			if i, ok := vs[0].GetValueType().(*firestorepb.Value_IntegerValue); ok && keepInt {
				if name == "abs" && i.IntegerValue < 0 {
					if i.IntegerValue == math.MinInt64 {
						return nil, evalErrf("abs: integer overflow")
					}
					return intValue(-i.IntegerValue), nil
				}
				return intValue(i.IntegerValue), nil
			}
			x, ok := asFloat(vs[0])
			if !ok {
				return nil, evalErrf("%s: %s is not a number", name, typeName(vs[0]))
			}
			return doubleValue(do(x)), nil
		})
	}
	unary("abs", true, math.Abs)
	unary("ceil", true, math.Ceil)
	unary("floor", true, math.Floor)
	unary("round", true, math.Round)
	unary("trunc", true, math.Trunc)
	unary("sqrt", false, math.Sqrt)
	unary("exp", false, math.Exp)
	unary("ln", false, math.Log)
	unary("log10", false, math.Log10)

	// Strings.
	str := func(name string, v *firestorepb.Value) (string, error) {
		s, ok := v.GetValueType().(*firestorepb.Value_StringValue)
		if !ok {
			return "", evalErrf("%s: %s is not a string", name, typeName(v))
		}
		return s.StringValue, nil
	}
	strFn := func(name string, n int, do func(ss []string) (*firestorepb.Value, error)) {
		simple(name, n, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			ss := make([]string, len(vs))
			for i, v := range vs {
				if v == nil || isNull(v) {
					return nullValue(), nil
				}
				s, err := str(name, v)
				if err != nil {
					return nil, err
				}
				ss[i] = s
			}
			return do(ss)
		})
	}
	strFn("char_length", 1, func(ss []string) (*firestorepb.Value, error) {
		return intValue(int64(utf8.RuneCountInString(ss[0]))), nil
	})
	simple("byte_length", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		switch x := vs[0].GetValueType().(type) {
		case *firestorepb.Value_StringValue:
			return intValue(int64(len(x.StringValue))), nil
		case *firestorepb.Value_BytesValue:
			return intValue(int64(len(x.BytesValue))), nil
		case nil, *firestorepb.Value_NullValue:
			return nullValue(), nil
		}
		return nil, evalErrf("byte_length: %s is not a string or bytes", typeName(vs[0]))
	})
	strFn("to_lower", 1, func(ss []string) (*firestorepb.Value, error) { return stringValue(strings.ToLower(ss[0])), nil })
	strFn("to_upper", 1, func(ss []string) (*firestorepb.Value, error) { return stringValue(strings.ToUpper(ss[0])), nil })
	trim := func(name string, do func(s, cut string) string, def func(string) string) {
		simple(name, -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if len(vs) < 1 || len(vs) > 2 {
				return nil, status.Errorf(codes.InvalidArgument, "the function %q takes 1 or 2 arguments.", name)
			}
			if vs[0] == nil || isNull(vs[0]) {
				return nullValue(), nil
			}
			s, err := str(name, vs[0])
			if err != nil {
				return nil, err
			}
			if len(vs) == 2 {
				cut, err := str(name, vs[1])
				if err != nil {
					return nil, err
				}
				return stringValue(do(s, cut)), nil
			}
			return stringValue(def(s)), nil
		})
	}
	trim("trim", strings.Trim, func(s string) string { return strings.TrimFunc(s, unicode.IsSpace) })
	trim("ltrim", strings.TrimLeft, func(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) })
	trim("rtrim", strings.TrimRight, func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) })
	strFn("starts_with", 2, func(ss []string) (*firestorepb.Value, error) { return boolValue(strings.HasPrefix(ss[0], ss[1])), nil })
	strFn("ends_with", 2, func(ss []string) (*firestorepb.Value, error) { return boolValue(strings.HasSuffix(ss[0], ss[1])), nil })
	strFn("string_contains", 2, func(ss []string) (*firestorepb.Value, error) { return boolValue(strings.Contains(ss[0], ss[1])), nil })
	strFn("string_concat", -1, func(ss []string) (*firestorepb.Value, error) { return stringValue(strings.Join(ss, "")), nil })
	strFn("string_reverse", 1, func(ss []string) (*firestorepb.Value, error) { return stringValue(reverseString(ss[0])), nil })
	strFn("string_index_of", 2, func(ss []string) (*firestorepb.Value, error) {
		i := strings.Index(ss[0], ss[1])
		if i < 0 {
			return intValue(-1), nil
		}
		return intValue(int64(utf8.RuneCountInString(ss[0][:i]))), nil
	})
	strFn("string_replace_all", 3, func(ss []string) (*firestorepb.Value, error) {
		return stringValue(strings.ReplaceAll(ss[0], ss[1], ss[2])), nil
	})
	strFn("string_replace_one", 3, func(ss []string) (*firestorepb.Value, error) {
		return stringValue(strings.Replace(ss[0], ss[1], ss[2], 1)), nil
	})
	simple("string_repeat", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		s, err := str("string_repeat", vs[0])
		if err != nil {
			return nil, err
		}
		n, ok := asInt(vs[1])
		if !ok || n < 0 || n*int64(len(s)) > 1<<20 {
			return nil, evalErrf("string_repeat: the count must be a non-negative integer")
		}
		return stringValue(strings.Repeat(s, int(n))), nil
	})
	strFn("split", 2, func(ss []string) (*firestorepb.Value, error) {
		parts := strings.Split(ss[0], ss[1])
		vals := make([]*firestorepb.Value, len(parts))
		for i, p := range parts {
			vals[i] = stringValue(p)
		}
		return arrayValue(vals), nil
	})
	re := func(name string, anchored bool) {
		strFn(name, 2, func(ss []string) (*firestorepb.Value, error) {
			pat := ss[1]
			if anchored {
				pat = `^(?:` + pat + `)$`
			}
			rx, err := regexp.Compile(pat)
			if err != nil {
				return nil, evalErrf("%s: invalid regular expression %q: %v", name, ss[1], err)
			}
			return boolValue(rx.MatchString(ss[0])), nil
		})
	}
	re("regex_contains", false)
	re("regex_match", true)
	strFn("like", 2, func(ss []string) (*firestorepb.Value, error) {
		var b strings.Builder
		b.WriteString("^(?s:")
		for _, c := range ss[1] {
			switch c {
			case '%':
				b.WriteString(".*")
			case '_':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
		b.WriteString(")$")
		return boolValue(regexp.MustCompile(b.String()).MatchString(ss[0])), nil
	})
	simple("substring", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs) < 2 || len(vs) > 3 {
			return nil, status.Error(codes.InvalidArgument, "the function \"substring\" takes 2 or 3 arguments.")
		}
		if vs[0] == nil || isNull(vs[0]) {
			return nullValue(), nil
		}
		s, err := str("substring", vs[0])
		if err != nil {
			return nil, err
		}
		rs := []rune(s)
		pos, ok := asInt(vs[1])
		if !ok {
			return nil, evalErrf("substring: the position must be an integer")
		}
		if pos < 0 {
			pos = max(int64(len(rs))+pos, 0)
		}
		pos = min(pos, int64(len(rs)))
		end := int64(len(rs))
		if len(vs) == 3 {
			n, ok := asInt(vs[2])
			if !ok || n < 0 {
				return nil, evalErrf("substring: the length must be a non-negative integer")
			}
			end = min(pos+n, end)
		}
		return stringValue(string(rs[pos:end])), nil
	})

	// Strings and arrays alike.
	simple("reverse", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		switch x := vs[0].GetValueType().(type) {
		case *firestorepb.Value_StringValue:
			return stringValue(reverseString(x.StringValue)), nil
		case *firestorepb.Value_ArrayValue:
			return arrayValue(reversed(x.ArrayValue.GetValues())), nil
		case nil, *firestorepb.Value_NullValue:
			return nullValue(), nil
		}
		return nil, evalErrf("reverse: %s is not a string or an array", typeName(vs[0]))
	})
	simple("length", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		switch x := vs[0].GetValueType().(type) {
		case *firestorepb.Value_StringValue:
			return intValue(int64(utf8.RuneCountInString(x.StringValue))), nil
		case *firestorepb.Value_BytesValue:
			return intValue(int64(len(x.BytesValue))), nil
		case *firestorepb.Value_ArrayValue:
			return intValue(int64(len(x.ArrayValue.GetValues()))), nil
		case *firestorepb.Value_MapValue:
			if isVector(vs[0]) {
				v, _ := vectorOf(vs[0])
				return intValue(int64(len(v))), nil
			}
			return intValue(int64(len(x.MapValue.GetFields()))), nil
		case nil, *firestorepb.Value_NullValue:
			return nullValue(), nil
		}
		return nil, evalErrf("length: %s has no length", typeName(vs[0]))
	})
	simple("concat", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs) == 0 {
			return nil, status.Error(codes.InvalidArgument, "the function \"concat\" takes at least 1 argument.")
		}
		switch vs[0].GetValueType().(type) {
		case *firestorepb.Value_ArrayValue:
			var out []*firestorepb.Value
			for _, v := range vs {
				a := v.GetArrayValue()
				if a == nil {
					return nil, evalErrf("concat: %s is not an array", typeName(v))
				}
				out = append(out, a.GetValues()...)
			}
			return arrayValue(out), nil
		case *firestorepb.Value_BytesValue:
			var out []byte
			for _, v := range vs {
				b, ok := v.GetValueType().(*firestorepb.Value_BytesValue)
				if !ok {
					return nil, evalErrf("concat: %s is not bytes", typeName(v))
				}
				out = append(out, b.BytesValue...)
			}
			return &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: out}}, nil
		}
		var b strings.Builder
		for _, v := range vs {
			if v == nil || isNull(v) {
				return nullValue(), nil
			}
			s, err := str("concat", v)
			if err != nil {
				return nil, err
			}
			b.WriteString(s)
		}
		return stringValue(b.String()), nil
	})

	// Arrays.
	arr := func(name string, v *firestorepb.Value) ([]*firestorepb.Value, error) {
		a := v.GetArrayValue()
		if a == nil {
			return nil, evalErrf("%s: %s is not an array", name, typeName(v))
		}
		return a.GetValues(), nil
	}
	simple("array", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		out := make([]*firestorepb.Value, len(vs))
		for i, v := range vs {
			if v == nil {
				v = nullValue()
			}
			out[i] = v
		}
		return arrayValue(out), nil
	})
	simple("array_contains", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil {
			return boolValue(false), nil
		}
		a, err := arr("array_contains", vs[0])
		if err != nil {
			return nil, err
		}
		for _, e := range a {
			if equalValues(e, vs[1]) {
				return boolValue(true), nil
			}
		}
		return boolValue(false), nil
	})
	containsSet := func(name string, all bool) {
		simple(name, 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil {
				return boolValue(false), nil
			}
			a, err := arr(name, vs[0])
			if err != nil {
				return nil, err
			}
			want, err := arr(name, vs[1])
			if err != nil {
				return nil, err
			}
			for _, w := range want {
				found := false
				for _, e := range a {
					if equalValues(e, w) {
						found = true
						break
					}
				}
				if found && !all {
					return boolValue(true), nil
				}
				if !found && all {
					return boolValue(false), nil
				}
			}
			return boolValue(all), nil
		})
	}
	containsSet("array_contains_any", false)
	containsSet("array_contains_all", true)
	simple("array_length", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil || isNull(vs[0]) {
			return nullValue(), nil
		}
		a, err := arr("array_length", vs[0])
		if err != nil {
			return nil, err
		}
		return intValue(int64(len(a))), nil
	})
	simple("array_reverse", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil || isNull(vs[0]) {
			return nullValue(), nil
		}
		a, err := arr("array_reverse", vs[0])
		if err != nil {
			return nil, err
		}
		return arrayValue(reversed(a)), nil
	})
	simple("array_concat", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		var out []*firestorepb.Value
		for _, v := range vs {
			if v == nil || isNull(v) {
				return nullValue(), nil
			}
			a, err := arr("array_concat", v)
			if err != nil {
				return nil, err
			}
			out = append(out, a...)
		}
		return arrayValue(out), nil
	})
	at := func(name string, pick func(a []*firestorepb.Value, vs []*firestorepb.Value) (*firestorepb.Value, error), n int) {
		simple(name, n, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil || isNull(vs[0]) {
				return nil, nil
			}
			a, err := arr(name, vs[0])
			if err != nil {
				return nil, err
			}
			return pick(a, vs)
		})
	}
	at("array_get", func(a, vs []*firestorepb.Value) (*firestorepb.Value, error) {
		i, ok := asInt(vs[1])
		if !ok {
			return nil, evalErrf("array_get: the offset must be an integer")
		}
		if i < 0 {
			i += int64(len(a))
		}
		if i < 0 || i >= int64(len(a)) {
			return nil, nil
		}
		return a[i], nil
	}, 2)
	at("array_first", func(a, _ []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(a) == 0 {
			return nil, nil
		}
		return a[0], nil
	}, 1)
	at("array_last", func(a, _ []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(a) == 0 {
			return nil, nil
		}
		return a[len(a)-1], nil
	}, 1)
	at("array_index_of", func(a, vs []*firestorepb.Value) (*firestorepb.Value, error) {
		for i, e := range a {
			if equalValues(e, vs[1]) {
				return intValue(int64(i)), nil
			}
		}
		return intValue(-1), nil
	}, 2)
	at("array_slice", func(a, vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs) < 2 || len(vs) > 3 {
			return nil, status.Error(codes.InvalidArgument, "the function \"array_slice\" takes 2 or 3 arguments.")
		}
		off, ok := asInt(vs[1])
		if !ok {
			return nil, evalErrf("array_slice: the offset must be an integer")
		}
		if off < 0 {
			off = max(int64(len(a))+off, 0)
		}
		off = min(off, int64(len(a)))
		end := int64(len(a))
		if len(vs) == 3 {
			n, ok := asInt(vs[2])
			if !ok || n < 0 {
				return nil, evalErrf("array_slice: the length must be a non-negative integer")
			}
			end = min(off+n, end)
		}
		return arrayValue(a[off:end]), nil
	}, -1)
	minmax := func(name string, sign int) {
		simple(name, -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			// logical_minimum and logical_maximum: the least or greatest
			// of the arguments, null and absent ignored.
			var best *firestorepb.Value
			for _, v := range vs {
				if v == nil || isNull(v) {
					continue
				}
				if best == nil || compareValues(v, best)*sign > 0 {
					best = v
				}
			}
			if best == nil {
				return nullValue(), nil
			}
			return best, nil
		})
	}
	minmax("logical_minimum", -1)
	minmax("logical_maximum", 1)
	simple("join", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil || isNull(vs[0]) {
			return nullValue(), nil
		}
		a, err := arr("join", vs[0])
		if err != nil {
			return nil, err
		}
		sep, err := str("join", vs[1])
		if err != nil {
			return nil, err
		}
		parts := make([]string, 0, len(a))
		for _, e := range a {
			if isNull(e) {
				continue
			}
			s, err := str("join", e)
			if err != nil {
				return nil, err
			}
			parts = append(parts, s)
		}
		return stringValue(strings.Join(parts, sep)), nil
	})

	// Maps.
	simple("map", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs)%2 != 0 {
			return nil, status.Error(codes.InvalidArgument, "the function \"map\" takes key and value pairs.")
		}
		out := map[string]*firestorepb.Value{}
		for i := 0; i < len(vs); i += 2 {
			k, err := str("map", vs[i])
			if err != nil {
				return nil, err
			}
			if vs[i+1] != nil {
				out[k] = vs[i+1]
			}
		}
		return mapValue(out), nil
	})
	mapOf := func(name string, v *firestorepb.Value) (map[string]*firestorepb.Value, error) {
		m := v.GetMapValue()
		if m == nil || isVector(v) {
			return nil, evalErrf("%s: %s is not a map", name, typeName(v))
		}
		return m.GetFields(), nil
	}
	simple("map_get", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil || isNull(vs[0]) {
			return nil, nil
		}
		m, err := mapOf("map_get", vs[0])
		if err != nil {
			return nil, err
		}
		k, err := str("map_get", vs[1])
		if err != nil {
			return nil, err
		}
		return m[k], nil
	})
	simple("get_field", 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		return pipelineFunctionsMapGet(vs)
	})
	simple("map_merge", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		out := map[string]*firestorepb.Value{}
		for _, v := range vs {
			m, err := mapOf("map_merge", v)
			if err != nil {
				return nil, err
			}
			for k, e := range m {
				out[k] = e
			}
		}
		return mapValue(out), nil
	})
	simple("map_set", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs) < 3 || len(vs)%2 == 0 {
			return nil, status.Error(codes.InvalidArgument, "the function \"map_set\" takes a map and key and value pairs.")
		}
		m, err := mapOf("map_set", vs[0])
		if err != nil {
			return nil, err
		}
		out := cloneFields(m)
		for i := 1; i < len(vs); i += 2 {
			k, err := str("map_set", vs[i])
			if err != nil {
				return nil, err
			}
			if vs[i+1] == nil {
				delete(out, k)
			} else {
				out[k] = vs[i+1]
			}
		}
		return mapValue(out), nil
	})
	simple("map_remove", -1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if len(vs) < 1 {
			return nil, status.Error(codes.InvalidArgument, "the function \"map_remove\" takes a map and keys.")
		}
		m, err := mapOf("map_remove", vs[0])
		if err != nil {
			return nil, err
		}
		out := cloneFields(m)
		for _, kv := range vs[1:] {
			k, err := str("map_remove", kv)
			if err != nil {
				return nil, err
			}
			delete(out, k)
		}
		return mapValue(out), nil
	})
	keysOrValues := func(name string, keys bool) {
		simple(name, 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			m, err := mapOf(name, vs[0])
			if err != nil {
				return nil, err
			}
			var out []*firestorepb.Value
			for _, k := range sortedKeys(m) {
				if keys {
					out = append(out, stringValue(k))
				} else {
					out = append(out, m[k])
				}
			}
			return arrayValue(out), nil
		})
	}
	keysOrValues("map_keys", true)
	keysOrValues("map_values", false)

	// Documents.
	pathOf := func(name string, v *firestorepb.Value) (string, error) {
		ref, ok := v.GetValueType().(*firestorepb.Value_ReferenceValue)
		if !ok {
			return "", evalErrf("%s: %s is not a reference", name, typeName(v))
		}
		return ref.ReferenceValue, nil
	}
	simple("document_id", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil {
			return nil, nil
		}
		p, err := pathOf("document_id", vs[0])
		if err != nil {
			return nil, err
		}
		return stringValue(p[strings.LastIndex(p, "/")+1:]), nil
	})
	simple("collection_id", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil {
			return nil, nil
		}
		p, err := pathOf("collection_id", vs[0])
		if err != nil {
			return nil, err
		}
		return stringValue(collectionOf(p)), nil
	})
	simple("parent", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		if vs[0] == nil {
			return nil, nil
		}
		p, err := pathOf("parent", vs[0])
		if err != nil {
			return nil, err
		}
		parts := strings.Split(p, "/")
		// projects/P/databases/D/documents/c/d: the parent of a
		// top-level document is none.
		if len(parts) <= 7 {
			return nullValue(), nil
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: strings.Join(parts[:len(parts)-2], "/")}}, nil
	})

	// Timestamps.
	f["current_timestamp"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) != 0 {
			return nil, status.Error(codes.InvalidArgument, "the function \"current_timestamp\" takes no arguments.")
		}
		return timestampValue(p.now), nil
	}
	units := map[string]time.Duration{
		"microsecond": time.Microsecond, "millisecond": time.Millisecond, "second": time.Second,
		"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour,
	}
	shift := func(name string, sign int64) {
		simple(name, 3, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil || isNull(vs[0]) {
				return nullValue(), nil
			}
			ts := vs[0].GetTimestampValue()
			if ts == nil {
				return nil, evalErrf("%s: %s is not a timestamp", name, typeName(vs[0]))
			}
			u, ok := units[vs[1].GetStringValue()]
			if !ok {
				return nil, evalErrf("%s: unit %q is invalid: want microsecond, millisecond, second, minute, hour or day", name, vs[1].GetStringValue())
			}
			n, ok := asInt(vs[2])
			if !ok {
				return nil, evalErrf("%s: the amount must be an integer", name)
			}
			return timestampValue(ts.AsTime().Add(time.Duration(sign*n) * u)), nil
		})
	}
	shift("timestamp_add", 1)
	shift("timestamp_subtract", -1)
	toUnix := func(name string, per time.Duration) {
		simple(name, 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil || isNull(vs[0]) {
				return nullValue(), nil
			}
			ts := vs[0].GetTimestampValue()
			if ts == nil {
				return nil, evalErrf("%s: %s is not a timestamp", name, typeName(vs[0]))
			}
			t := ts.AsTime()
			return intValue(t.Unix()*int64(time.Second/per) + int64(t.Nanosecond())/int64(per)), nil
		})
	}
	toUnix("timestamp_to_unix_seconds", time.Second)
	toUnix("timestamp_to_unix_millis", time.Millisecond)
	toUnix("timestamp_to_unix_micros", time.Microsecond)
	fromUnix := func(name string, per time.Duration) {
		simple(name, 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			if vs[0] == nil || isNull(vs[0]) {
				return nullValue(), nil
			}
			n, ok := asInt(vs[0])
			if !ok {
				return nil, evalErrf("%s: %s is not an integer", name, typeName(vs[0]))
			}
			k := int64(time.Second / per)
			sec, rem := n/k, n%k
			if rem < 0 {
				sec, rem = sec-1, rem+k
			}
			return timestampValue(time.Unix(sec, rem*int64(per)).UTC()), nil
		})
	}
	fromUnix("unix_seconds_to_timestamp", time.Second)
	fromUnix("unix_millis_to_timestamp", time.Millisecond)
	fromUnix("unix_micros_to_timestamp", time.Microsecond)

	// Vectors.
	vec := func(name string, do func(a, b []float64) (float64, bool)) {
		simple(name, 2, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
			a, ok1 := vectorOf(vs[0])
			b, ok2 := vectorOf(vs[1])
			if !ok1 || !ok2 {
				return nil, evalErrf("%s: both arguments must be vectors", name)
			}
			if len(a) != len(b) {
				return nil, evalErrf("%s: the vectors have %d and %d dimensions", name, len(a), len(b))
			}
			d, ok := do(a, b)
			if !ok {
				return nil, evalErrf("%s: a vector has zero magnitude", name)
			}
			return doubleValue(d), nil
		})
	}
	vec("cosine_distance", func(a, b []float64) (float64, bool) { return distance("cosine", a, b) })
	vec("euclidean_distance", func(a, b []float64) (float64, bool) { return distance("euclidean", a, b) })
	vec("dot_product", func(a, b []float64) (float64, bool) { return distance("dot_product", a, b) })
	simple("vector_length", 1, func(vs []*firestorepb.Value) (*firestorepb.Value, error) {
		v, ok := vectorOf(vs[0])
		if !ok {
			return nil, evalErrf("vector_length: %s is not a vector", typeName(vs[0]))
		}
		return intValue(int64(len(v))), nil
	})
}

// pipelineFunctionsMapGet is map_get, which get_field shares.
func pipelineFunctionsMapGet(vs []*firestorepb.Value) (*firestorepb.Value, error) {
	if vs[0] == nil || isNull(vs[0]) {
		return nil, nil
	}
	m := vs[0].GetMapValue()
	if m == nil {
		return nil, evalErrf("get_field: %s is not a map", typeName(vs[0]))
	}
	return m.GetFields()[vs[1].GetStringValue()], nil
}

func reverseString(s string) string {
	rs := []rune(s)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs)
}

func reversed(vs []*firestorepb.Value) []*firestorepb.Value {
	out := make([]*firestorepb.Value, len(vs))
	for i, v := range vs {
		out[len(vs)-1-i] = v
	}
	return out
}
