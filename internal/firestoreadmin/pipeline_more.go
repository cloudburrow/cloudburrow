package firestoreadmin

// More of Google's pipeline operations (#1523): the literals and let
// stages, the write stages update and delete, the search stage's
// geographic queries, and the regex, array, timestamp, lambda and
// geographic functions.
//
// The write stages write through the emulator's BatchWrite, in batches of
// at most pipelineWriteBatch documents, each document written atomically
// on its own, as Google documents: "Each document is updated
// independently. This means operations are not atomic across documents",
// and "The operation fails on first error and partial success is
// possible".
//
// The search stage's full-text queries (document_matches, score) stay
// refused: Google's text search reference does not define its tokenization
// or its score, so no equivalent can be built from it.

import (
	"math"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // timestamp functions take IANA time zones

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pipelineWriteBatch is how many documents one BatchWrite of a write
// stage carries: Firestore's limit on a batch's writes.
const pipelineWriteBatch = 500

// earthRadiusMeters is the mean Earth radius geo_distance measures over,
// that of S2, the geometry library Google's geographic indexes use.
const earthRadiusMeters = 6371010.0

func pipelineWrites(pl *firestorepb.Pipeline) bool {
	for _, st := range pl.GetStages() {
		if st.GetName() == "update" || st.GetName() == "delete" {
			return true
		}
	}
	return false
}

// checkStagePlace enforces where Google's reference lets a stage stand:
// a write stage last, after none of the stages it lists as unsupported
// before one, and search right after the input stage.
func checkStagePlace(stages []*firestorepb.Pipeline_Stage, i int) error {
	name := stages[i].GetName()
	switch name {
	case "update", "delete":
		if i != len(stages)-1 {
			return status.Errorf(codes.InvalidArgument, "the %s stage must be the pipeline's last stage: DML stages must be the last stages in a pipeline definition.", name)
		}
		for _, st := range stages[:i] {
			switch st.GetName() {
			case "aggregate", "distinct", "unnest", "find_nearest", "union", "search":
				return status.Errorf(codes.InvalidArgument, "the %s stage is not supported before a %s stage.", st.GetName(), name)
			}
		}
	case "search":
		if i != 1 {
			return status.Error(codes.InvalidArgument, "the search stage must directly follow the pipeline's input stage.")
		}
	}
	return nil
}

// literals is the literals input stage: each argument, a map, is a
// document with no name.
func (p *pipelineRun) literals(args []*firestorepb.Value) ([]row, error) {
	out := make([]row, 0, len(args))
	for _, a := range args {
		v, err := p.evalStrict(a, row{})
		if err != nil {
			return nil, err
		}
		m := v.GetMapValue()
		if m == nil {
			return nil, stageErr("literals", "each document is a map, not %s.", typeName(v))
		}
		r := row{fields: cloneFields(m.GetFields())}
		if n := r.fields["__name__"].GetReferenceValue(); n != "" {
			r.name = n
			delete(r.fields, "__name__")
		}
		out = append(out, r)
	}
	return out, nil
}

// let binds each alias of vars to its expression's value, for the later
// stages to read with a variable reference.
func (p *pipelineRun) let(vars *firestorepb.Value, rows []row) ([]row, error) {
	defs := vars.GetMapValue().GetFields()
	if len(defs) == 0 {
		return nil, stageErr("let", "at least one variable is required.")
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		n := r
		n.vars = make(map[string]*firestorepb.Value, len(r.vars)+len(defs))
		for k, v := range r.vars {
			n.vars[k] = v
		}
		for _, alias := range sortedKeys(defs) {
			v, err := p.evalStrict(defs[alias], r)
			if err != nil {
				return nil, err
			}
			if v == nil {
				v = nullValue()
			}
			n.vars[alias] = v
		}
		out = append(out, n)
	}
	return out, nil
}

// withVar is r with the variable name bound to v.
func (r row) withVar(name string, v *firestorepb.Value) row {
	n := r
	n.vars = make(map[string]*firestorepb.Value, len(r.vars)+1)
	for k, x := range r.vars {
		n.vars[k] = x
	}
	if v == nil {
		v = nullValue()
	}
	n.vars[name] = v
	return n
}

// write is the update and delete stages. update replaces each document
// with the row (after the transformations, evaluated as add_fields would
// be), failing if it does not exist; delete deletes it, a repeat being a
// no-op. The result is one document: {documents_modified: N}.
func (p *pipelineRun) write(name string, transforms *firestorepb.Value, rows []row) ([]row, error) {
	var writes []*firestorepb.Write
	seen := map[string]bool{}
	for _, r := range rows {
		if r.name == "" {
			return nil, stageErr(name, "the documents coming into this stage must include the __name__ field to identify which documents to %s.", name)
		}
		if name == "delete" {
			if seen[r.name] {
				continue
			}
			seen[r.name] = true
			writes = append(writes, &firestorepb.Write{Operation: &firestorepb.Write_Delete{Delete: r.name}})
			continue
		}
		fields := cloneFields(r.fields)
		tf := transforms.GetMapValue().GetFields()
		for _, alias := range sortedKeys(tf) {
			v, err := p.evalStrict(tf[alias], r)
			if err != nil {
				return nil, err
			}
			if v != nil {
				setPath(fields, splitPath(alias), v)
			} else {
				deletePath(fields, splitPath(alias))
			}
		}
		writes = append(writes, &firestorepb.Write{
			Operation:       &firestorepb.Write_Update{Update: &firestorepb.Document{Name: r.name, Fields: fields}},
			CurrentDocument: &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_Exists{Exists: true}},
		})
	}
	if !p.plan && len(writes) > 0 {
		c, err := p.a.docs()
		if err != nil {
			return nil, err
		}
		ctx := asOwner(p.ctx)
		// A batch holds each document once: a document an update stage
		// meets again starts the next batch, so it is written again,
		// after the first.
		for len(writes) > 0 {
			in := map[string]bool{}
			n := 0
			for n < len(writes) && n < pipelineWriteBatch {
				doc := writes[n].GetUpdate().GetName() + writes[n].GetDelete()
				if in[doc] {
					break
				}
				in[doc] = true
				n++
			}
			resp, err := c.BatchWrite(ctx, &firestorepb.BatchWriteRequest{Database: p.db, Writes: writes[:n]})
			if err != nil {
				return nil, err
			}
			for i, st := range resp.GetStatus() {
				if codes.Code(st.GetCode()) != codes.OK {
					doc := writes[i].GetUpdate().GetName() + writes[i].GetDelete()
					if codes.Code(st.GetCode()) == codes.NotFound {
						return nil, status.Errorf(codes.NotFound, "%s: the document %s does not exist.", name, doc)
					}
					return nil, status.Errorf(codes.Code(st.GetCode()), "%s of %s: %s", name, doc, st.GetMessage())
				}
			}
			writes = writes[n:]
		}
	}
	modified := len(rows)
	if name == "delete" {
		modified = len(seen)
	}
	return []row{{fields: map[string]*firestorepb.Value{"documents_modified": intValue(int64(modified))}}}, nil
}

// usesTextSearch reports whether v calls a full-text search function.
func usesTextSearch(v *firestorepb.Value) bool {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_FunctionValue:
		if n := x.FunctionValue.GetName(); n == "document_matches" || n == "score" {
			return true
		}
		for _, a := range x.FunctionValue.GetArgs() {
			if usesTextSearch(a) {
				return true
			}
		}
	case *firestorepb.Value_ArrayValue:
		for _, a := range x.ArrayValue.GetValues() {
			if usesTextSearch(a) {
				return true
			}
		}
	case *firestorepb.Value_MapValue:
		for _, a := range x.MapValue.GetFields() {
			if usesTextSearch(a) {
				return true
			}
		}
	}
	return false
}

// search is the search stage over its geographic queries: the rows
// matching the query, newest first unless sorted, with the added fields,
// after the offset and up to the limit.
func (p *pipelineRun) search(opts map[string]*firestorepb.Value, rows []row) ([]row, error) {
	for k, v := range opts {
		switch k {
		case "query", "sort", "add_fields", "limit", "offset", "retrieval_depth", "language_code":
		default:
			return nil, stageErr("search", "unknown option %q.", k)
		}
		if usesTextSearch(v) {
			return nil, status.Error(codes.Unimplemented, "full-text search (document_matches, score) is not served here: it needs a text index, and Google's text search reference does not define its tokenization or its score, so CloudBurrow has no equivalent; geographic search (geo_distance) is served")
		}
	}
	var out []row
	for _, r := range rows {
		if q := opts["query"]; q != nil {
			v, err := p.eval(q, r)
			if err != nil {
				if isEvalError(err) {
					continue
				}
				return nil, err
			}
			if !v.GetBooleanValue() {
				continue
			}
		}
		out = append(out, r)
	}
	// By default, Firestore sorts results by document creation time,
	// newest to oldest.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].create.AsTime().After(out[j].create.AsTime())
	})
	if d, ok := asInt(opts["retrieval_depth"]); ok && d >= 0 && int(d) < len(out) {
		out = out[:d]
	}
	var err error
	if s := opts["sort"]; s != nil {
		if out, err = p.stage(&firestorepb.Pipeline_Stage{Name: "sort", Args: s.GetArrayValue().GetValues()}, out); err != nil {
			return nil, err
		}
	}
	if af := opts["add_fields"]; af != nil {
		if out, err = p.stage(&firestorepb.Pipeline_Stage{Name: "add_fields", Args: []*firestorepb.Value{af}}, out); err != nil {
			return nil, err
		}
	}
	if o, ok := asInt(opts["offset"]); ok && o > 0 {
		out = out[min(int(o), len(out)):]
	}
	if l, ok := asInt(opts["limit"]); ok && l >= 0 && int(l) < len(out) {
		out = out[:l]
	}
	return out, nil
}

// --- the functions ---

func absent(v *firestorepb.Value) bool { return v == nil || isNull(v) }

func init() {
	f := pipelineFunctions
	for _, n := range []string{"regex_find", "regex_find_all", "array_first_n", "array_last_n", "array_index_of_all",
		"minimum_n", "maximum_n", "array_filter", "array_transform", "timestamp_trunc", "timestamp_diff",
		"timestamp_extract", "geo_distance", "current_document", "map_entries", "cmp", "switch_on", "rand", "offset"} {
		delete(pipelineUnserved, n)
	}
	argc := func(name string, args []*firestorepb.Value, lo, hi int) error {
		if len(args) < lo || len(args) > hi {
			if lo == hi {
				return status.Errorf(codes.InvalidArgument, "the function %q takes %d arguments, not %d.", name, lo, len(args))
			}
			return status.Errorf(codes.InvalidArgument, "the function %q takes %d to %d arguments, not %d.", name, lo, hi, len(args))
		}
		return nil
	}
	evals := func(p *pipelineRun, args []*firestorepb.Value, r row) ([]*firestorepb.Value, error) {
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
	// def registers a function of lo to hi evaluated arguments, null when
	// its first is absent or null.
	def := func(name string, lo, hi int, do func(p *pipelineRun, vs []*firestorepb.Value, r row) (*firestorepb.Value, error)) {
		f[name] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
			if err := argc(name, args, lo, hi); err != nil {
				return nil, err
			}
			vs, err := evals(p, args, r)
			if err != nil {
				return nil, err
			}
			if len(vs) > 0 && absent(vs[0]) {
				return nullValue(), nil
			}
			return do(p, vs, r)
		}
	}
	array := func(name string, v *firestorepb.Value) ([]*firestorepb.Value, error) {
		a := v.GetArrayValue()
		if a == nil {
			return nil, evalErrf("%s: %s is not an array", name, typeName(v))
		}
		return a.GetValues(), nil
	}
	count := func(name string, v *firestorepb.Value) (int, error) {
		n, ok := asInt(v)
		if !ok || n < 0 {
			return 0, evalErrf("%s: n must be a non-negative integer", name)
		}
		return int(n), nil
	}

	// Regular expressions (RE2): the first match, and every match; with
	// one capturing group, its text.
	regex := func(name string, all bool) {
		def(name, 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
			s, ok := vs[0].GetValueType().(*firestorepb.Value_StringValue)
			if !ok {
				return nil, evalErrf("%s: %s is not a string", name, typeName(vs[0]))
			}
			rx, err := regexp.Compile(vs[1].GetStringValue())
			if err != nil {
				return nil, evalErrf("%s: invalid regular expression %q: %v", name, vs[1].GetStringValue(), err)
			}
			if rx.NumSubexp() > 1 {
				return nil, evalErrf("%s: the regular expression has more than one capturing group", name)
			}
			matches := rx.FindAllStringSubmatch(s.StringValue, -1)
			pick := func(m []string) *firestorepb.Value { return stringValue(m[len(m)-1]) }
			if !all {
				if len(matches) == 0 {
					return nullValue(), nil
				}
				return pick(matches[0]), nil
			}
			out := make([]*firestorepb.Value, 0, len(matches))
			for _, m := range matches {
				out = append(out, pick(m))
			}
			return arrayValue(out), nil
		})
	}
	regex("regex_find", false)
	regex("regex_find_all", true)

	// Arrays.
	firstLast := func(name string, last bool) {
		def(name, 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
			a, err := array(name, vs[0])
			if err != nil {
				return nil, err
			}
			n, err := count(name, vs[1])
			if err != nil {
				return nil, err
			}
			n = min(n, len(a))
			if last {
				return arrayValue(a[len(a)-n:]), nil
			}
			return arrayValue(a[:n]), nil
		})
	}
	firstLast("array_first_n", false)
	firstLast("array_last_n", true)
	def("array_index_of_all", 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		a, err := array("array_index_of_all", vs[0])
		if err != nil {
			return nil, err
		}
		out := []*firestorepb.Value{}
		for i, e := range a {
			if equalValues(e, vs[1]) {
				out = append(out, intValue(int64(i)))
			}
		}
		return arrayValue(out), nil
	})
	extremes := func(name string, desc bool) {
		def(name, 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
			a, err := array(name, vs[0])
			if err != nil {
				return nil, err
			}
			n, err := count(name, vs[1])
			if err != nil {
				return nil, err
			}
			var keep []*firestorepb.Value
			for _, e := range a {
				if !isNull(e) {
					keep = append(keep, e)
				}
			}
			sort.SliceStable(keep, func(i, j int) bool {
				c := compareValues(keep[i], keep[j])
				if desc {
					return c > 0
				}
				return c < 0
			})
			return arrayValue(keep[:min(n, len(keep))]), nil
		})
	}
	extremes("minimum_n", false)
	extremes("maximum_n", true)
	def("offset", 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		a := vs[0].GetArrayValue()
		i, ok := asInt(vs[1])
		if a == nil || !ok {
			return nil, nil
		}
		if i < 0 {
			i += int64(len(a.GetValues()))
		}
		if i < 0 || i >= int64(len(a.GetValues())) {
			return nil, nil
		}
		return a.GetValues()[i], nil
	})

	// Lambdas: array_filter(array, param, body) and
	// array_transform(array, param[, index param], body), the body
	// evaluated with each element (and its index) bound to the params.
	lambda := func(name string, args []*firestorepb.Value, lo, hi int) (params []string, body *firestorepb.Value, err error) {
		if err := argc(name, args, lo, hi); err != nil {
			return nil, nil, err
		}
		for _, a := range args[1 : len(args)-1] {
			s := a.GetStringValue()
			if s == "" {
				return nil, nil, status.Errorf(codes.InvalidArgument, "%s: each parameter is a constant name.", name)
			}
			params = append(params, s)
		}
		return params, args[len(args)-1], nil
	}
	f["array_filter"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		params, body, err := lambda("array_filter", args, 3, 3)
		if err != nil {
			return nil, err
		}
		v, err := p.eval(args[0], r)
		if err != nil || absent(v) {
			return nullValue(), err
		}
		a, err := array("array_filter", v)
		if err != nil {
			return nil, err
		}
		out := []*firestorepb.Value{}
		for _, e := range a {
			ok, err := p.eval(body, r.withVar(params[0], e))
			if err != nil {
				return nil, err
			}
			if ok.GetBooleanValue() {
				out = append(out, e)
			}
		}
		return arrayValue(out), nil
	}
	f["array_transform"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		params, body, err := lambda("array_transform", args, 3, 4)
		if err != nil {
			return nil, err
		}
		v, err := p.eval(args[0], r)
		if err != nil || absent(v) {
			return nullValue(), err
		}
		a, err := array("array_transform", v)
		if err != nil {
			return nil, err
		}
		out := make([]*firestorepb.Value, 0, len(a))
		for i, e := range a {
			rr := r.withVar(params[0], e)
			if len(params) == 2 {
				rr = rr.withVar(params[1], intValue(int64(i)))
			}
			x, err := p.eval(body, rr)
			if err != nil {
				return nil, err
			}
			if x == nil {
				x = nullValue()
			}
			out = append(out, x)
		}
		return arrayValue(out), nil
	}

	// Timestamps.
	zone := func(name string, vs []*firestorepb.Value, i int) (*time.Location, error) {
		if i >= len(vs) || absent(vs[i]) {
			return time.UTC, nil
		}
		loc, err := time.LoadLocation(vs[i].GetStringValue())
		if err != nil {
			return nil, evalErrf("%s: invalid time zone %q", name, vs[i].GetStringValue())
		}
		return loc, nil
	}
	tsOf := func(name string, v *firestorepb.Value) (time.Time, error) {
		ts := v.GetTimestampValue()
		if ts == nil {
			return time.Time{}, evalErrf("%s: %s is not a timestamp", name, typeName(v))
		}
		return ts.AsTime(), nil
	}
	weekday := func(part, base string) (time.Weekday, bool) {
		if part == base {
			return time.Sunday, true
		}
		if !strings.HasPrefix(part, base+"(") || !strings.HasSuffix(part, ")") {
			return 0, false
		}
		day := strings.TrimSuffix(strings.TrimPrefix(part, base+"("), ")")
		for d := time.Sunday; d <= time.Saturday; d++ {
			if strings.EqualFold(d.String(), day) {
				return d, true
			}
		}
		return 0, false
	}
	midnight := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()) }
	weekStart := func(t time.Time, start time.Weekday) time.Time {
		d := midnight(t)
		return d.AddDate(0, 0, -((int(d.Weekday()) - int(start) + 7) % 7))
	}
	isoYearStart := func(y int, loc *time.Location) time.Time {
		// The ISO year starts on the Monday of the week holding 4 January.
		return weekStart(time.Date(y, time.January, 4, 0, 0, 0, 0, loc), time.Monday)
	}
	def("timestamp_trunc", 2, 3, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		t, err := tsOf("timestamp_trunc", vs[0])
		if err != nil {
			return nil, err
		}
		loc, err := zone("timestamp_trunc", vs, 2)
		if err != nil {
			return nil, err
		}
		t = t.In(loc)
		g := strings.ToLower(vs[1].GetStringValue())
		var out time.Time
		switch g {
		case "microsecond":
			out = t.Truncate(time.Microsecond)
		case "millisecond":
			out = t.Truncate(time.Millisecond)
		case "second":
			out = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
		case "minute":
			out = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		case "hour":
			out = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		case "day":
			out = midnight(t)
		case "isoweek":
			out = weekStart(t, time.Monday)
		case "month":
			out = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		case "quarter":
			out = time.Date(t.Year(), time.Month((int(t.Month())-1)/3*3+1), 1, 0, 0, 0, 0, loc)
		case "year":
			out = time.Date(t.Year(), time.January, 1, 0, 0, 0, 0, loc)
		case "isoyear":
			y, _ := t.ISOWeek()
			out = isoYearStart(y, loc)
		default:
			d, ok := weekday(g, "week")
			if !ok {
				return nil, evalErrf("timestamp_trunc: granularity %q is invalid", g)
			}
			out = weekStart(t, d)
		}
		return timestampValue(out), nil
	})
	def("timestamp_extract", 2, 3, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		t, err := tsOf("timestamp_extract", vs[0])
		if err != nil {
			return nil, err
		}
		loc, err := zone("timestamp_extract", vs, 2)
		if err != nil {
			return nil, err
		}
		t = t.In(loc)
		part := strings.ToLower(vs[1].GetStringValue())
		var n int
		switch part {
		case "microsecond":
			n = t.Nanosecond() / 1000
		case "millisecond":
			n = t.Nanosecond() / 1e6
		case "second":
			n = t.Second()
		case "minute":
			n = t.Minute()
		case "hour":
			n = t.Hour()
		case "day":
			n = t.Day()
		case "dayofweek":
			n = int(t.Weekday()) + 1 // 1 is Sunday
		case "dayofyear":
			n = t.YearDay()
		case "month":
			n = int(t.Month())
		case "quarter":
			n = (int(t.Month())-1)/3 + 1
		case "year":
			n = t.Year()
		case "isoweek":
			_, n = t.ISOWeek()
		case "isoyear":
			n, _ = t.ISOWeek()
		default:
			d, ok := weekday(part, "week")
			if !ok {
				return nil, evalErrf("timestamp_extract: part %q is invalid", part)
			}
			// The week number, weeks starting on day d: the days before
			// the year's first d are in week 0.
			first := time.Date(t.Year(), time.January, 1, 0, 0, 0, 0, loc)
			first = first.AddDate(0, 0, (int(d)-int(first.Weekday())+7)%7)
			if t.Before(first) {
				n = 0
			} else {
				n = int(midnight(t).Sub(first).Hours()/24)/7 + 1
			}
		}
		return intValue(int64(n)), nil
	})
	units := map[string]time.Duration{
		"microsecond": time.Microsecond, "millisecond": time.Millisecond, "second": time.Second,
		"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour,
	}
	def("timestamp_diff", 3, 3, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		if absent(vs[1]) {
			return nullValue(), nil
		}
		end, err := tsOf("timestamp_diff", vs[0])
		if err != nil {
			return nil, err
		}
		start, err := tsOf("timestamp_diff", vs[1])
		if err != nil {
			return nil, err
		}
		u, ok := units[strings.ToLower(vs[2].GetStringValue())]
		if !ok {
			return nil, evalErrf("timestamp_diff: unit %q is invalid: want microsecond, millisecond, second, minute, hour or day", vs[2].GetStringValue())
		}
		return intValue(int64(end.Sub(start) / u)), nil
	})

	// Geography: the great-circle distance in meters between two
	// GeoPoints.
	def("geo_distance", 2, 2, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		a, b := vs[0].GetGeoPointValue(), vs[1].GetGeoPointValue()
		if a == nil || b == nil {
			return nil, evalErrf("geo_distance: both arguments must be GeoPoints")
		}
		rad := func(d float64) float64 { return d * math.Pi / 180 }
		dLat, dLng := rad(b.GetLatitude()-a.GetLatitude()), rad(b.GetLongitude()-a.GetLongitude())
		h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(a.GetLatitude()))*math.Cos(rad(b.GetLatitude()))*math.Sin(dLng/2)*math.Sin(dLng/2)
		return doubleValue(2 * earthRadiusMeters * math.Asin(math.Min(1, math.Sqrt(h)))), nil
	})

	// The rest of the reference's general functions.
	f["current_document"] = func(_ *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if err := argc("current_document", args, 0, 0); err != nil {
			return nil, err
		}
		return mapValue(cloneFields(r.fields)), nil
	}
	f["rand"] = func(_ *pipelineRun, args []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		if err := argc("rand", args, 0, 0); err != nil {
			return nil, err
		}
		return doubleValue(rand.Float64()), nil
	}
	def("map_entries", 1, 1, func(_ *pipelineRun, vs []*firestorepb.Value, _ row) (*firestorepb.Value, error) {
		m := vs[0].GetMapValue()
		if m == nil {
			return nil, evalErrf("map_entries: %s is not a map", typeName(vs[0]))
		}
		out := []*firestorepb.Value{}
		for _, k := range sortedKeys(m.GetFields()) {
			out = append(out, mapValue(map[string]*firestorepb.Value{"k": stringValue(k), "v": m.GetFields()[k]}))
		}
		return arrayValue(out), nil
	})
	f["cmp"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if err := argc("cmp", args, 2, 2); err != nil {
			return nil, err
		}
		vs, err := evals(p, args, r)
		if err != nil {
			return nil, err
		}
		if vs[0] == nil || vs[1] == nil {
			return nullValue(), nil
		}
		return intValue(int64(compareValues(vs[0], vs[1]))), nil
	}
	f["switch_on"] = func(p *pipelineRun, args []*firestorepb.Value, r row) (*firestorepb.Value, error) {
		if len(args) < 2 {
			return nil, status.Error(codes.InvalidArgument, "the function \"switch_on\" takes at least a condition and its result.")
		}
		for i := 0; i+1 < len(args); i += 2 {
			c, err := p.eval(args[i], r)
			if err != nil {
				return nil, err
			}
			if c.GetBooleanValue() {
				return p.eval(args[i+1], r)
			}
		}
		if len(args)%2 == 1 {
			return p.eval(args[len(args)-1], r)
		}
		return nullValue(), nil
	}
}
