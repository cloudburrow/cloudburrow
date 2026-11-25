// Command dataflowgo is the Go SDK pipelines of the Dataflow compat tests
// (test/compat/dataflowgo2215_test.go, #2215), written with Apache Beam's
// Go SDK and submitted with its Dataflow runner (--runner=dataflow), as
// against Google. The same binary is the job's worker (--worker_binary).
//
//	--pipeline=wordcount  counts the words of --input into --output
//	--pipeline=pubsub     copies --subscription's messages to --topic,
//	                      upper-cased (streaming)
//	--pipeline=bigquery   writes three rows to --table through the
//	                      cross-language BigQuery write, and reads them
//	                      back into --output through the cross-language
//	                      read (Beam's Java expansion service at
//	                      --expansion_addr)
//
// With --bigquery_endpoint, the cross-language transforms' expansion
// requests carry it as the pipeline option bigQueryEndpoint, which the Java
// expansion service's BigQueryIO looks the read's table schema up at. The
// Java and Python SDKs send their pipeline options with every expansion
// request; the Go SDK sends none of its own (its flags are not Java's), so
// the pipeline adds it through an expansion handler of its own (xlangx's
// RegisterHandler), as Beam documents for custom expansion.
package main

import (
	"context"
	"flag"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/apache/beam/sdks/v2/go/pkg/beam"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/core/runtime/xlangx"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/pubsubio"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/textio"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/xlang/bigqueryio"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/log"
	jobpb "github.com/apache/beam/sdks/v2/go/pkg/beam/model/jobmanagement_v1"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/register"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/transforms/stats"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/x/beamx"
	"google.golang.org/protobuf/types/known/structpb"

	_ "github.com/apache/beam/sdks/v2/go/pkg/beam/io/filesystem/gcs"
)

var (
	pipeline      = flag.String("pipeline", "wordcount", "wordcount, pubsub or bigquery")
	input         = flag.String("input", "", "wordcount: the text to count")
	output        = flag.String("output", "", "wordcount and bigquery: where the results go")
	subscription  = flag.String("subscription", "", "pubsub: the subscription read")
	topic         = flag.String("topic", "", "pubsub: the topic written")
	table         = flag.String("table", "", "bigquery: PROJECT:DATASET.TABLE")
	expansionAddr = flag.String("expansion_addr", "", "bigquery: the Java expansion service")
	project       = flag.String("pubsub_project", "", "pubsub: the project of --subscription and --topic")
	bqEndpoint    = flag.String("bigquery_endpoint", "", "bigquery: the BigQuery endpoint the expansion service's BigQueryIO uses")
)

// withBigQueryEndpoint is the expansion handler of the namespace
// "bigqueryendpoint": the expansion service's request with the pipeline
// option bigQueryEndpoint (and the project), sent to the service at its
// address.
func withBigQueryEndpoint(ctx context.Context, p *xlangx.HandlerParams) (*jobpb.ExpansionResponse, error) {
	opts := map[string]any{"beam:option:big_query_endpoint:v1": *bqEndpoint}
	if f := flag.Lookup("project"); f != nil && f.Value.String() != "" {
		opts["beam:option:project:v1"] = f.Value.String()
	}
	st, err := structpb.NewStruct(opts)
	if err != nil {
		return nil, err
	}
	if p.Req.PipelineOptions != nil {
		for k, v := range p.Req.PipelineOptions.Fields {
			if _, ok := st.Fields[k]; !ok {
				st.Fields[k] = v
			}
		}
	}
	p.Req.PipelineOptions = st
	return xlangx.QueryExpansionService(ctx, p)
}

// expansion is --expansion_addr, through withBigQueryEndpoint when
// --bigquery_endpoint is set.
func expansion() string {
	if *bqEndpoint == "" {
		return *expansionAddr
	}
	return "bigqueryendpoint" + xlangx.Separator + *expansionAddr
}

var wordRE = regexp.MustCompile(`[a-zA-Z]+`)

// Score is one BigQuery row.
type Score struct {
	Name  string `beam:"name"`
	Score int64  `beam:"score"`
}

var linesCounter = beam.NewCounter("dataflowgo", "lines")

func extractFn(ctx context.Context, line string, emit func(string)) {
	linesCounter.Inc(ctx, 1)
	for _, w := range wordRE.FindAllString(line, -1) {
		emit(w)
	}
}

func formatFn(w string, c int) string { return fmt.Sprintf("%s: %d", w, c) }

func upperFn(b []byte) []byte { return []byte(strings.ToUpper(string(b))) }

func scoresFn(_ []byte, emit func(Score)) {
	for i, n := range []string{"ada", "grace", "linus"} {
		emit(Score{Name: n, Score: int64(10 * (i + 1))})
	}
}

// formatScoreFn formats a row the cross-language read produced. The Go
// harness decodes the Java read's schema rows by their schema alone, as an
// unnamed struct of the table's columns (pointers for NULLABLE ones), not
// as the Score the read asked for (Apache Beam issue 21784; Beam's own
// xlang BigQuery test reads into unnamed struct types for this reason), so
// the row's name and score fields are read by reflection.
func formatScoreFn(row beam.X) string {
	v := reflect.Indirect(reflect.ValueOf(row))
	field := func(name string) any {
		f := v.FieldByName(name)
		if !f.IsValid() {
			return nil
		}
		if f.Kind() == reflect.Pointer {
			if f.IsNil() {
				return nil
			}
			f = f.Elem()
		}
		return f.Interface()
	}
	return fmt.Sprintf("%v=%v", field("Name"), field("Score"))
}

func init() {
	register.Function3x0(extractFn)
	register.Emitter1[string]()
	register.Function2x1(formatFn)
	register.Function1x1(upperFn)
	register.Function2x0(scoresFn)
	register.Emitter1[Score]()
	register.Function1x1(formatScoreFn)
	beam.RegisterType(reflect.TypeOf((*Score)(nil)).Elem())
}

func main() {
	flag.Parse()
	xlangx.RegisterHandler("bigqueryendpoint", withBigQueryEndpoint)
	beam.Init()
	ctx := context.Background()
	p, s := beam.NewPipelineWithRoot()
	switch *pipeline {
	case "wordcount":
		lines := textio.Read(s, *input)
		words := beam.ParDo(s, extractFn, lines)
		counted := stats.Count(s, words)
		textio.Write(s, *output, beam.ParDo(s, formatFn, counted))
	case "pubsub":
		msgs := pubsubio.Read(s, *project, pubsubio.ReadOptions{Subscription: *subscription})
		pubsubio.Write(s, *project, *topic, beam.ParDo(s, upperFn, msgs))
	case "bigquery":
		w := s.Scope("Write")
		rows := beam.ParDo(w, scoresFn, beam.Impulse(w))
		bigqueryio.Write(w, *table, rows, bigqueryio.CreateDisposition(bigqueryio.CreateIfNeeded),
			bigqueryio.WriteExpansionAddr(expansion()))
	case "bigquery-read":
		read := bigqueryio.Read(s, reflect.TypeOf((*Score)(nil)).Elem(), bigqueryio.FromTable(*table),
			bigqueryio.ReadExpansionAddr(expansion()))
		textio.Write(s, *output, beam.ParDo(s, formatScoreFn, read))
	default:
		log.Exitf(ctx, "unknown --pipeline %q", *pipeline)
	}
	if err := beamx.Run(ctx, p); err != nil {
		log.Exitf(ctx, "run: %v", err)
	}
}
