package firestoreadmin

// Firestore with MongoDB compatibility (#1738): Query Explain, as Google
// documents it ("Analyze query execution with Query Explain",
// https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/query-explain,
// and its "Query execution reference"):
//
//   - the explain command, of find, aggregate, count, distinct, update,
//     delete and findAndModify; aggregate's own explain: true is refused,
//     as Google refuses it ("Query Explain does not support commands which
//     return a cursor");
//   - the verbosities queryPlanner (the plan alone, the query not run),
//     executionStats and allPlansExecution (the plan, and the statistics
//     of running it: results returned, records scanned, data bytes read,
//     peak memory, read units). An explained write writes nothing;
//   - the plan is an execution tree of Google's nodes: a TableScan of the
//     collection (the documents are scanned whole here; an index changes
//     no plan), with its filter, then MajorSort, Limit and Compute nodes as
//     the command needs them, and the same tree as Google's text.
//
// Google documents the plan's nodes and its summary statistics, as the
// console and mongosh print them, but not the reply's BSON fields; the
// reply is MongoDB's (queryPlanner.winningPlan, executionStats), carrying
// Google's tree and statistics.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// explainable are the commands Google's Query Explain supports.
var explainable = map[string]bool{"find": true, "aggregate": true, "count": true, "distinct": true,
	"update": true, "delete": true, "findandmodify": true}

// planNode is a node of an execution tree.
type planNode struct {
	stage    string
	props    bson.D
	returned int
	child    *planNode
}

func (n *planNode) bson(stats bool) bson.D {
	d := bson.D{{Key: "stage", Value: n.stage}}
	d = append(d, n.props...)
	if stats {
		d = append(d, bson.E{Key: "recordsReturned", Value: int64(n.returned)})
	}
	if n.child != nil {
		d = append(d, bson.E{Key: "inputStage", Value: n.child.bson(stats)})
	}
	return d
}

// text is the tree as Google prints it.
func (n *planNode) text(b *strings.Builder, indent string, stats bool) {
	fmt.Fprintf(b, "%s• %s\n", indent, n.stage)
	for _, p := range n.props {
		fmt.Fprintf(b, "%s|  %s: %v\n", indent, p.Key, p.Value)
	}
	if stats {
		fmt.Fprintf(b, "%s|\n%s|  Execution:\n%s|   records returned: %d\n", indent, indent, indent, n.returned)
	}
	if n.child != nil {
		fmt.Fprintf(b, "%s|\n%s└── ", indent, indent)
		var sub strings.Builder
		n.child.text(&sub, "", stats)
		lines := strings.Split(strings.TrimSuffix(sub.String(), "\n"), "\n")
		b.WriteString(lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(indent + "    " + l + "\n")
		}
	}
}

func filterText(f bson.D) string {
	if len(f) == 0 {
		return "true"
	}
	raw, err := bson.MarshalExtJSON(f, false, false)
	if err != nil {
		return fmt.Sprint(f)
	}
	return string(raw)
}

func (s *MongoServer) explain(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	inner, ok := cmd[0].Value.(bson.D)
	if !ok || len(inner) == 0 {
		return nil, mql.Errorf(14, "TypeMismatch", "BSON field 'explain.explain' is the wrong type '%s', expected type 'object'", mql.TypeName(cmd[0].Value))
	}
	verbosity := "allPlansExecution"
	if v, ok := mql.LookupE(cmd, "verbosity"); ok {
		vs, _ := v.(string)
		switch vs {
		case "queryPlanner", "executionStats", "allPlansExecution":
			verbosity = vs
		default:
			return nil, mql.BadValue("verbosity string must be one of {'queryPlanner', 'executionStats', 'allPlansExecution'}")
		}
	}
	iname := strings.ToLower(inner[0].Key)
	if !explainable[iname] {
		return nil, mql.Errorf(115, "CommandNotSupported", "Query Explain is only supported on the find, aggregate, count, distinct, update, delete and findAndModify commands, not %s.", inner[0].Key)
	}
	if err := checkUnsupported(iname, inner[0].Key, inner); err != nil {
		return nil, err
	}
	coll, named := inner[0].Value.(string)
	if named {
		if err := mql.CheckCollectionName(coll); err != nil {
			return nil, err
		}
	} else if iname != "aggregate" {
		return nil, mql.Errorf(73, "InvalidNamespace", "collection name has invalid type %s", mql.TypeName(inner[0].Value))
	}

	// The command's filter, sort, skip and limit.
	var filter bson.D
	var sortV any
	var skip, limit int
	var pipeline bson.A
	var err error
	switch iname {
	case "find":
		filter, err = filterOf(inner, "filter")
		sortV, skip, limit = mql.Lookup(inner, "sort"), intField(inner, "skip"), intField(inner, "limit")
		if limit < 0 {
			limit = -limit
		}
	case "count", "distinct":
		filter, err = filterOf(inner, "query")
		skip, limit = intField(inner, "skip"), intField(inner, "limit")
		if limit < 0 {
			limit = -limit
		}
	case "findandmodify":
		filter, err = filterOf(inner, "query")
		sortV, limit = mql.Lookup(inner, "sort"), 1
	case "update", "delete":
		key := "updates"
		if iname == "delete" {
			key = "deletes"
		}
		stmts, _ := mql.Lookup(inner, key).(bson.A)
		if len(stmts) != 1 {
			return nil, mql.BadValue("Explain of a %s needs exactly one statement, not %d.", inner[0].Key, len(stmts))
		}
		st, _ := stmts[0].(bson.D)
		filter, err = filterOf(st, "q")
		if iname == "update" {
			sortV = mql.Lookup(st, "sort")
			if !mql.Truthy(mql.Lookup(st, "multi")) {
				limit = 1
			}
		} else {
			limit = intField(st, "limit")
		}
	case "aggregate":
		var ok bool
		if pipeline, ok = mql.Lookup(inner, "pipeline").(bson.A); !ok {
			return nil, mql.Errorf(14, "TypeMismatch", "'pipeline' option must be specified as an array")
		}
		if len(pipeline) > 0 {
			if st, ok := pipeline[0].(bson.D); ok && len(st) == 1 && st[0].Key == "$match" {
				filter, _ = st[0].Value.(bson.D)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	srt, err := mql.ParseSort(sortV)
	if err != nil {
		return nil, err
	}

	run := verbosity != "queryPlanner"
	start := time.Now()
	var scanned, matched, returned, bytesRead int
	if run {
		var docs []bson.D
		if named {
			if docs, _, err = s.load(ctx, mc, coll); err != nil {
				return nil, err
			}
		} else if docs, pipeline, err = mql.DocumentsInput(pipeline); err != nil {
			return nil, err
		}
		scanned = len(docs)
		for _, d := range docs {
			raw, _ := bson.Marshal(d)
			bytesRead += len(raw)
		}
		switch iname {
		case "aggregate":
			env := mql.NewEnv(func(other string) ([]bson.D, error) {
				if err := mql.CheckCollectionName(other); err != nil {
					return nil, err
				}
				d, _, err := s.load(ctx, mc, other)
				return d, err
			})
			out, err := mql.RunPipeline(env, docs, pipeline)
			if err != nil {
				return nil, err
			}
			returned = len(out)
			if len(filter) > 0 {
				for _, d := range docs {
					if m, _ := mql.Matches(d, filter); m {
						matched++
					}
				}
			} else {
				matched = scanned
			}
		default:
			for _, d := range docs {
				m, err := mql.Matches(d, filter)
				if err != nil {
					return nil, err
				}
				if m {
					matched++
				}
			}
			returned = matched - skip
			if returned < 0 {
				returned = 0
			}
			if limit > 0 && limit < returned {
				returned = limit
			}
		}
	}
	elapsed := time.Since(start)

	// The tree, from the scan up.
	source := "**/" + coll
	if !named {
		source = "$documents"
	}
	scanNode := &planNode{stage: "TableScan", returned: matched, props: bson.D{{Key: "source", Value: source}, {Key: "order", Value: "STABLE"},
		{Key: "properties", Value: "*"}}}
	if len(filter) > 0 {
		scanNode.props = append(scanNode.props, bson.E{Key: "filter", Value: filterText(filter)})
	}
	if run {
		scanNode.props = append(scanNode.props, bson.E{Key: "recordsScanned", Value: int64(scanned)},
			bson.E{Key: "postFilteredRows", Value: int64(scanned - matched)}, bson.E{Key: "dataBytesRead", Value: int64(bytesRead)})
	}
	top := scanNode
	if len(srt) > 0 {
		var fields []string
		if sd, ok := sortV.(bson.D); ok {
			for _, e := range sd {
				dir := "ASC"
				if n, _ := mql.NumberOf(e.Value); n < 0 {
					dir = "DESC"
				}
				fields = append(fields, e.Key+" "+dir)
			}
		}
		top = &planNode{stage: "MajorSort", returned: matched, props: bson.D{{Key: "fields", Value: "[" + strings.Join(fields, ", ") + "]"}}, child: top}
	}
	if skip > 0 || limit > 0 {
		props := bson.D{}
		if skip > 0 {
			props = append(props, bson.E{Key: "offset", Value: int64(skip)})
		}
		if limit > 0 {
			props = append(props, bson.E{Key: "limit", Value: int64(limit)})
		}
		top = &planNode{stage: "Limit", returned: returned, props: props, child: top}
	}
	if iname == "aggregate" && len(pipeline) > 0 {
		var stages []string
		for _, st := range pipeline {
			if sd, ok := st.(bson.D); ok && len(sd) == 1 {
				stages = append(stages, sd[0].Key)
			}
		}
		top = &planNode{stage: "Compute", returned: returned, props: bson.D{{Key: "stages", Value: strings.Join(stages, ", ")}}, child: top}
	}
	top = &planNode{stage: "Compute", returned: returned, props: bson.D{{Key: "isQueryResult", Value: true}}, child: top}

	readUnits := (bytesRead + 4095) / 4096
	if run && readUnits == 0 {
		readUnits = 1
	}
	var text strings.Builder
	if run {
		fmt.Fprintf(&text, "Execution:\n results returned: %d\n request peak memory usage: %d B\n data bytes read: %d B\n entity row scanned: %d\n\nBilling:\n read units: %d\n\n",
			returned, bytesRead, bytesRead, scanned, readUnits)
	}
	text.WriteString("Tree:\n")
	top.text(&text, "", run)

	ns := mc.db.id + "." + coll
	resp := bson.D{{Key: "queryPlanner", Value: bson.D{{Key: "namespace", Value: ns}, {Key: "parsedQuery", Value: filter},
		{Key: "winningPlan", Value: top.bson(false)}, {Key: "rejectedPlans", Value: bson.A{}}}}}
	if run {
		resp = append(resp, bson.E{Key: "executionStats", Value: bson.D{{Key: "executionSuccess", Value: true},
			{Key: "nReturned", Value: int64(returned)}, {Key: "executionTimeMillis", Value: elapsed.Milliseconds()},
			{Key: "totalKeysExamined", Value: int64(0)}, {Key: "totalDocsExamined", Value: int64(scanned)},
			{Key: "resultsReturned", Value: int64(returned)}, {Key: "requestPeakMemoryUsageBytes", Value: int64(bytesRead)},
			{Key: "dataBytesRead", Value: int64(bytesRead)}, {Key: "entityRowsScanned", Value: int64(scanned)},
			{Key: "billing", Value: bson.D{{Key: "readUnits", Value: int64(readUnits)}}},
			{Key: "executionStages", Value: top.bson(true)}}})
	}
	resp = append(resp, bson.E{Key: "explainText", Value: text.String()},
		bson.E{Key: "command", Value: inner}, bson.E{Key: "verbosity", Value: verbosity})
	return resp, nil
}
