package bigqueryfront

// Stored procedures for Apache Spark (#2104).
//
// As the DDL reference documents them
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_procedure,
// re-read 2026-11-21) and the page that shows them
// (https://docs.cloud.google.com/bigquery/docs/spark-procedures):
//
//	CREATE [OR REPLACE] PROCEDURE [IF NOT EXISTS] name(args)
//	WITH CONNECTION project.region.connection
//	[OPTIONS(procedure_option_list)]
//	LANGUAGE PYTHON|JAVA|SCALA [AS pyspark_code]
//
// The procedure is kept beside the SQL ones (procedures.go), its Routine
// resource of language PYTHON, JAVA or SCALA, its definitionBody the
// inline code and its sparkOptions the options (connection as
// projects/P/locations/L/connections/C, runtimeVersion, mainFileUri,
// mainClass, pyFileUris, jarUris, fileUris, archiveUris, properties).
// Checked when it is made: engine="SPARK" is required; the inline code
// "Cannot appear with main_file_uri" and a PySpark procedure has one of
// them; Java and Scala ones have exactly one of main_file_uri and
// main_class, and no inline code; py_file_uris "Applies only to stored
// procedures for Apache Spark written in Python"; a YARN property is
// refused as the page's example shows ("Invalid properties: Attempted to
// set unsupported properties: [spark:spark.yarn.am.memory]"); the
// connection must exist, be a spark connection, and be in the dataset's
// location ("Creation and calls must share a location with the
// connection"), and the caller needs bigquery.connections.delegate on it.
//
// CALL runs it on the CLI's Spark runner (internal/sparkproc): the
// procedure's files are read from the instance's Cloud Storage, which the
// connection's service account needs storage.objects.get on; the caller
// needs bigquery.connections.use. Each argument is passed as the
// BIGQUERY_PROC_PARAM.<name> environment variable, JSON-encoded, and
// through SparkProcParamContext; an OUT or INOUT one a PySpark procedure
// sets is written back to the caller's variable. The CALL is a child job
// of its script whose statistics.query.sparkStatistics give its
// sparkJobId, sparkJobLocation and loggingInfo (projectId, resourceType
// bigquery.googleapis.com/SparkJob), which name its entries in the
// instance's Cloud Logging. A failed application fails the CALL.
//
// Not carried out (501): EXTERNAL SECURITY INVOKER (a custom service
// account), container_image (a custom container), and runtime versions
// whose Spark is not 3.5 (3.0 and 4.0); the served ones are 1.2, 2.2 (the
// default) and 2.3, whose Spark is the pinned 3.5 image's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// sparkRunner runs a Spark procedure's CALL: *sparkproc.Client.
type sparkRunner interface {
	Run(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error)
}

// sparkDef is what a Spark procedure keeps beyond its Routine.
type sparkDef struct {
	Language   string            `json:"language"`
	Code       string            `json:"code,omitempty"`
	Connection []string          `json:"connection"` // project, location, id
	Options    map[string]any    `json:"options"`    // sparkOptions
	Properties map[string]string `json:"properties,omitempty"`
}

const (
	permConnectionsDelegate = "bigquery.connections.delegate"
	sparkResourceType       = "bigquery.googleapis.com/SparkJob"
)

// sparkRuntimes are the served runtime versions: those whose Spark is 3.5.
var sparkRuntimes = map[string]bool{"1.2": true, "2.2": true, "2.3": true}

// sparkHeader is a Spark CREATE PROCEDURE, read.
type sparkHeader struct {
	procHeader
	invoker    bool
	connection []string
	language   string
	code       string
	hasCode    bool
	options    map[string]sparkOptValue
}

// sparkOptValue is one OPTIONS value: a string, an array of strings, or
// an array of (key, value) pairs.
type sparkOptValue struct {
	str   string
	list  []string
	pairs [][2]string
	kind  string // string, list, pairs
}

// isSparkHeader reports whether a CREATE PROCEDURE's header, up to its
// end, is a Spark procedure's (WITH CONNECTION or LANGUAGE at its top).
func isSparkHeader(t []token) bool {
	depth := 0
	for _, x := range t {
		switch {
		case x.punct("(") || x.punct("["):
			depth++
		case x.punct(")") || x.punct("]"):
			depth--
		case depth == 0 && (x.is("LANGUAGE") || x.is("CONNECTION")):
			return true
		}
	}
	return false
}

// readSparkHeader reads a Spark CREATE PROCEDURE statement.
func readSparkHeader(sql string, t []token) (sparkHeader, int, string) {
	var h sparkHeader
	ph, msg := readProcHeader(sql, cutAtLanguage(t))
	if msg != "" {
		return h, 400, msg
	}
	h.procHeader = ph
	h.unsupported = ""
	if strings.Contains(ph.unsupported, "TEMP") {
		return h, 501, "Not implemented here: a TEMP procedure"
	}
	h.options = map[string]sparkOptValue{}
	// After the arguments' closing parenthesis.
	i := 0
	for i < len(t) && !t[i].punct("(") {
		i++
	}
	if end := closing(t, i); end > 0 {
		i = end + 1
	}
	for i < len(t) {
		switch {
		case t[i].is("EXTERNAL") && i+2 < len(t) && t[i+1].is("SECURITY"):
			if !t[i+2].is("INVOKER") {
				return h, 400, "Syntax error: EXTERNAL SECURITY takes INVOKER"
			}
			h.invoker = true
			i += 3
		case t[i].is("WITH") && i+1 < len(t) && t[i+1].is("CONNECTION"):
			parts, next := path(t, i+2)
			if len(parts) == 1 && strings.Count(parts[0], ".") == 2 {
				parts = strings.Split(parts[0], ".")
			}
			if len(parts) != 3 {
				return h, 400, "Syntax error: WITH CONNECTION takes connection_project_id.connection_region.connection_id"
			}
			h.connection = parts
			i = next
		case t[i].is("OPTIONS") && i+1 < len(t) && t[i+1].punct("("):
			e := closing(t, i+1)
			if e < 0 {
				return h, 400, "Syntax error in OPTIONS"
			}
			if code, msg := readSparkOptions(t[i+2:e], h.options); code != 0 {
				return h, code, msg
			}
			i = e + 1
		case t[i].is("LANGUAGE") && i+1 < len(t):
			h.language = strings.ToUpper(t[i+1].text)
			i += 2
			if i < len(t) && t[i].is("AS") {
				if i+1 >= len(t) || t[i+1].kind != tokString {
					return h, 400, "Syntax error: expected the procedure's code, a string literal, after AS"
				}
				h.code, h.hasCode = unquoteLiteral(t[i+1].text), true
				i += 2
			}
		case t[i].punct(";"):
			i++
		default:
			return h, 400, fmt.Sprintf("Syntax error: unexpected %q in CREATE PROCEDURE", t[i].text)
		}
	}
	return h, 0, ""
}

// cutAtLanguage is t up to its first top-level LANGUAGE or WITH
// CONNECTION, the part readProcHeader reads.
func cutAtLanguage(t []token) []token {
	depth := 0
	for i, x := range t {
		switch {
		case x.punct("(") || x.punct("["):
			depth++
		case x.punct(")") || x.punct("]"):
			depth--
		case depth == 0 && (x.is("LANGUAGE") || x.is("WITH") || x.is("EXTERNAL")):
			return t[:i]
		}
	}
	return t
}

// readSparkOptions reads OPTIONS' name = value pairs.
func readSparkOptions(t []token, into map[string]sparkOptValue) (int, string) {
	for _, item := range splitTop(t) {
		if len(item) == 0 {
			continue
		}
		if len(item) < 3 || !item[1].punct("=") {
			return 400, "Syntax error in OPTIONS: expected NAME = VALUE"
		}
		name := strings.ToLower(item[0].text)
		v := item[2:]
		var val sparkOptValue
		switch {
		case len(v) == 1 && v[0].kind == tokString:
			val = sparkOptValue{kind: "string", str: unquoteLiteral(v[0].text)}
		case len(v) == 1 && (v[0].is("TRUE") || v[0].is("FALSE")):
			val = sparkOptValue{kind: "string", str: strings.ToLower(v[0].text)}
		case len(v) >= 2 && v[0].punct("[") && v[len(v)-1].punct("]"):
			inner := v[1 : len(v)-1]
			if len(inner) > 0 && inner[0].punct("(") {
				val.kind = "pairs"
				for _, p := range splitTop(inner) {
					if len(p) == 0 {
						continue
					}
					if len(p) != 5 || !p[0].punct("(") || p[1].kind != tokString || !p[2].punct(",") || p[3].kind != tokString || !p[4].punct(")") {
						return 400, "Invalid value for OPTIONS " + name + ": it is an ARRAY<STRUCT<STRING, STRING>>, [(\"key1\",\"value1\"),(\"key2\", \"value2\")]"
					}
					val.pairs = append(val.pairs, [2]string{unquoteLiteral(p[1].text), unquoteLiteral(p[3].text)})
				}
				break
			}
			val.kind = "list"
			for _, e := range splitTop(inner) {
				if len(e) == 0 {
					continue
				}
				if len(e) != 1 || e[0].kind != tokString {
					return 400, "Invalid value for OPTIONS " + name + ": it is an ARRAY<STRING>"
				}
				val.list = append(val.list, unquoteLiteral(e[0].text))
			}
		default:
			return 400, "Invalid value for OPTIONS " + name + ": it is a literal"
		}
		into[name] = val
	}
	return 0, ""
}

// sparkOptionKinds are the procedure options a Spark procedure takes, and
// each one's kind.
var sparkOptionKinds = map[string]string{
	"engine": "string", "runtime_version": "string", "container_image": "string", "properties": "pairs",
	"main_file_uri": "string", "main_class": "string", "py_file_uris": "list", "jar_uris": "list",
	"file_uris": "list", "archive_uris": "list", "description": "string", "strict_mode": "string",
}

// createSparkProc carries out a Spark CREATE PROCEDURE.
func (s *scriptRun) createSparkProc(n scriptNode) *scriptFlow {
	text := s.sql[n.procSpan[0]:n.procSpan[1]]
	h, code, msg := readSparkHeader(s.sql, n.toks)
	if code == 501 {
		s.notImpl = "Not implemented here: " + strings.TrimPrefix(msg, "Not implemented here: ") + ". Nothing more was run."
		return &scriptFlow{}
	}
	if code != 0 {
		return failWith(code, "invalidQuery", msg, text)
	}
	fail := func(m string) *scriptFlow { return failWith(http.StatusBadRequest, "invalidQuery", m, text) }
	for name, v := range h.options {
		kind, ok := sparkOptionKinds[name]
		if !ok {
			return fail("Invalid procedure option " + name + " for a stored procedure for Apache Spark")
		}
		if v.kind != kind {
			return fail("Invalid value for procedure option " + name)
		}
	}
	opt := func(name string) string { return h.options[name].str }
	if !strings.EqualFold(opt("engine"), "SPARK") {
		return fail(`The engine option must be specified for stored procedures for Spark: engine="SPARK"`)
	}
	if h.connection == nil {
		return fail("A stored procedure for Apache Spark needs WITH CONNECTION")
	}
	lang := h.language
	switch lang {
	case "PYTHON", "JAVA", "SCALA":
	case "":
		return fail("A stored procedure for Apache Spark needs LANGUAGE PYTHON, JAVA or SCALA")
	default:
		return fail("Unsupported language " + lang + ": stored procedures for Apache Spark are written in Python, Java or Scala")
	}
	mainFile, mainClass := opt("main_file_uri"), opt("main_class")
	switch {
	case h.hasCode && lang != "PYTHON":
		return fail("Inline code is supported only for stored procedures for Apache Spark written in Python")
	case h.hasCode && mainFile != "":
		return fail("pyspark_code cannot appear with main_file_uri in procedure_option_list")
	case lang == "PYTHON" && !h.hasCode && mainFile == "":
		return fail("A stored procedure for Apache Spark written in Python has its code inline (AS) or in main_file_uri")
	case lang == "PYTHON" && mainClass != "":
		return fail("main_class applies only to stored procedures for Spark written in Java and Scala")
	case lang != "PYTHON" && (mainFile == "") == (mainClass == ""):
		return fail("You can set only one value for main_file_uri and main_class")
	case lang != "PYTHON" && len(h.options["py_file_uris"].list) > 0:
		return fail("py_file_uris applies only to stored procedures for Apache Spark written in Python")
	case mainClass != "" && len(h.options["jar_uris"].list) == 0:
		return fail("main_class is a class in the JARs set with the jar_uris option")
	}
	for _, u := range append(append(append(append([]string{mainFile}, h.options["py_file_uris"].list...), h.options["jar_uris"].list...),
		h.options["file_uris"].list...), h.options["archive_uris"].list...) {
		if u == "" {
			continue
		}
		if b, o, ok := strings.Cut(strings.TrimPrefix(u, "gs://"), "/"); !strings.HasPrefix(u, "gs://") || !ok || b == "" || o == "" {
			return fail("Invalid Cloud Storage URI " + u + ": it is gs://bucket/object")
		}
	}
	props := map[string]string{}
	var bad []string
	for _, p := range h.options["properties"].pairs {
		if strings.HasPrefix(p[0], "spark.yarn.") || strings.HasPrefix(p[0], "yarn:") {
			bad = append(bad, "spark:"+p[0])
		}
		props[p[0]] = p[1]
	}
	if len(bad) > 0 {
		return fail("Invalid value: Invalid properties: Attempted to set unsupported properties: [" + strings.Join(bad, ", ") + "]")
	}
	switch rv := opt("runtime_version"); {
	case h.invoker:
		s.notImpl = "Not implemented here: EXTERNAL SECURITY INVOKER (a stored procedure for Apache Spark that runs as a custom " +
			"service account). CloudBurrow runs Spark procedures as their connection's service account. Nothing more was run."
		return &scriptFlow{}
	case opt("container_image") != "":
		s.notImpl = "Not implemented here: container_image (a custom container for a stored procedure for Apache Spark). " +
			"CloudBurrow runs Spark procedures in its pinned Apache Spark 3.5 image. Nothing more was run."
		return &scriptFlow{}
	case rv != "" && !sparkRuntimes[sparkRuntimeMinor(rv)]:
		if m := sparkRuntimeMinor(rv); m == "3.0" || m == "4.0" {
			s.notImpl = "Not implemented here: runtime_version " + rv + " (Apache Spark 4). CloudBurrow runs stored procedures for " +
				"Apache Spark on Spark 3.5: runtime versions 1.2, 2.2 and 2.3. Nothing more was run."
			return &scriptFlow{}
		}
		return fail("Invalid runtime_version " + rv + ": the supported runtime versions are 1.2, 2.2, 2.3, 3.0 and 4.0")
	case s.f.spark == nil:
		s.notImpl = "Not implemented here: stored procedures for Apache Spark need the CLI's Spark runner, which this BigQuery " +
			"front was not given. Nothing more was run."
		return &scriptFlow{}
	}

	p, flow := s.procPath(h.path, text)
	if flow != nil {
		return flow
	}
	project := s.project()
	status, dsBody := s.f.get(s.r, "/datasets/"+url.PathEscape(p[0]))
	if status == http.StatusNotFound {
		return failWith(http.StatusNotFound, "notFound", "Not found: Dataset "+project+":"+p[0], text)
	}
	var ds struct {
		Location string `json:"location"`
	}
	_ = json.Unmarshal(dsBody, &ds)
	cn, code, msg := s.f.sparkConnection(h.connection)
	if code != 0 {
		return failWith(code, map[int]string{400: "invalidQuery", 403: "accessDenied", 404: "notFound"}[code], msg, text)
	}
	if ds.Location != "" && !sameLocation(cn.Location, ds.Location) {
		return fail(fmt.Sprintf("The procedure's dataset location %s differs from the connection location %s: creation and calls must share a location with the connection",
			ds.Location, cn.Location))
	}
	if code, msg := s.f.checkConnectionPerm(s.r, h.connection[0], cn, permConnectionsDelegate); code != 0 {
		return failWith(code, "accessDenied", msg, text)
	}
	_, exists := s.f.functions.proc(project, p)
	if !exists {
		_, exists = s.f.functions.defsIn(project, p[0])[strings.ToLower(p[1])]
		exists = exists && s.f.routineExists(s.r, p[0], p[1])
	}
	switch {
	case exists && h.ifNotExists:
		return s.assigned()
	case exists && !h.replace:
		return failWith(http.StatusConflict, "duplicate", "Already Exists: Routine "+project+":"+p[0]+"."+p[1], text)
	}
	spark := map[string]any{"connection": "projects/" + h.connection[0] + "/locations/" + strings.ToLower(h.connection[1]) + "/connections/" + h.connection[2]}
	for name, field := range map[string]string{"runtime_version": "runtimeVersion", "main_file_uri": "mainFileUri", "main_class": "mainClass"} {
		if v := opt(name); v != "" {
			spark[field] = v
		}
	}
	for name, field := range map[string]string{"py_file_uris": "pyFileUris", "jar_uris": "jarUris", "file_uris": "fileUris", "archive_uris": "archiveUris"} {
		if v := h.options[name].list; len(v) > 0 {
			spark[field] = v
		}
	}
	if len(props) > 0 {
		spark["properties"] = props
	}
	now := strconv.FormatInt(s.f.now().UnixMilli(), 10)
	routine := map[string]any{
		"routineReference": map[string]any{"projectId": project, "datasetId": p[0], "routineId": p[1]},
		"routineType":      "PROCEDURE", "language": lang, "sparkOptions": spark,
		"creationTime": now, "lastModifiedTime": now, "etag": etagOf(now + p[1]),
	}
	if h.hasCode {
		routine["definitionBody"] = h.code
	}
	if old, ok := s.f.functions.proc(project, p); ok {
		if c, ok := old.Routine["creationTime"]; ok {
			routine["creationTime"] = c
		}
	}
	if d := opt("description"); d != "" {
		routine["description"] = d
	}
	var args []map[string]any
	for _, a := range h.args {
		arg := map[string]any{"name": a.Name, "mode": a.Mode}
		if tt, ok := lex(a.Type); ok {
			if typ, _ := sqlTypeAt(tt, 0); typ != nil {
				arg["dataType"] = typ
			}
		}
		args = append(args, arg)
	}
	if len(args) > 0 {
		routine["arguments"] = args
	}
	if s.dry {
		return s.assigned()
	}
	s.f.functions.setProc(project, p, storedProc{Routine: routine, Args: h.args,
		Spark: &sparkDef{Language: lang, Code: h.code, Connection: h.connection, Options: spark, Properties: props}})
	return s.assigned()
}

// sparkRuntimeMinor is a runtime version's MAJOR.MINOR ("2.2.30" is 2.2).
func sparkRuntimeMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// sparkConnection is the spark connection parts names.
func (f front) sparkConnection(parts []string) (*bqConnection, int, string) {
	number := connectionProject(parts[0], f.numbers)
	cn, ok := f.connections.lookup(number, parts[1], parts[2])
	name := parts[0] + "." + strings.ToLower(parts[1]) + "." + parts[2]
	if !ok {
		return nil, 404, "Not found: Connection " + name
	}
	if _, ok := cn.Fields["spark"]; !ok {
		return nil, 400, "The connection " + name + " is not a Spark connection: a stored procedure for Apache Spark needs a connection of type SPARK"
	}
	return cn, 0, ""
}

// sparkServiceAccount is a spark connection's service account.
func (cn *bqConnection) sparkServiceAccount() string {
	var props struct {
		ServiceAccountID string `json:"serviceAccountId"`
	}
	_ = json.Unmarshal(cn.Fields["spark"], &props)
	return props.ServiceAccountID
}

// callSpark carries out CALL of a Spark procedure: scope holds the IN and
// INOUT arguments' values, outs the OUT and INOUT ones' caller variables.
func (s *scriptRun) callSpark(proc storedProc, path []string, project string, scope map[string]scriptVar, outs map[string]string,
	stmt string) *scriptFlow {
	def := proc.Spark
	fail := func(code int, reason, m string) *scriptFlow { return failWith(code, reason, m, stmt) }
	if s.f.spark == nil {
		s.notImpl = "Not implemented here: stored procedures for Apache Spark need the CLI's Spark runner, which this BigQuery " +
			"front was not given. Nothing more was run."
		return &scriptFlow{}
	}
	cn, code, msg := s.f.sparkConnection(def.Connection)
	if code != 0 {
		return fail(code, map[int]string{400: "invalidQuery", 404: "notFound"}[code], msg)
	}
	if code, msg := s.f.checkConnectionPerm(s.r, def.Connection[0], cn, permConnectionsUse); code != 0 {
		return fail(code, "accessDenied", msg)
	}
	sa := cn.sparkServiceAccount()
	job := sparkproc.Job{Project: project, Location: strings.ToLower(cn.Location), Procedure: project + "." + path[0] + "." + path[1],
		Language: def.Language, Code: def.Code, Properties: def.Properties, ServiceAccount: sa, Params: map[string]string{}}
	read := func(uri string) (sparkproc.File, *scriptFlow) {
		if s.f.storage == nil {
			s.notImpl = "Not implemented here: a stored procedure for Apache Spark reads its files from the instance's Cloud Storage, " +
				"which the BigQuery front was not given (enable Cloud Storage). Nothing more was run."
			return sparkproc.File{}, &scriptFlow{}
		}
		bucket, name, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
		if code, msg := s.f.checkStoragePerm(s.r, sa, bucket, permObjectsGet); code != 0 {
			return sparkproc.File{}, fail(code, "accessDenied", msg)
		}
		rd, err := s.f.storage.open(s.r.Context(), gsObject{bucket, name})
		if err != nil {
			return sparkproc.File{}, fail(http.StatusBadRequest, "invalid", "Cannot read "+uri+": "+err.Error())
		}
		defer rd.Close()
		b, err := io.ReadAll(io.LimitReader(rd, sparkproc.MaxJobBytes))
		if err != nil {
			return sparkproc.File{}, fail(http.StatusBadRequest, "invalid", "Cannot read "+uri+": "+err.Error())
		}
		return sparkproc.File{URI: uri, Data: b}, nil
	}
	list := func(field string) []string {
		var out []string
		switch v := def.Options[field].(type) {
		case []string:
			out = v
		case []any:
			for _, x := range v {
				if str, ok := x.(string); ok {
					out = append(out, str)
				}
			}
		}
		return out
	}
	if u, _ := def.Options["mainFileUri"].(string); u != "" {
		f, flow := read(u)
		if flow != nil {
			return flow
		}
		job.Main = &f
	}
	job.MainClass, _ = def.Options["mainClass"].(string)
	for field, dst := range map[string]*[]sparkproc.File{"pyFileUris": &job.PyFiles, "jarUris": &job.Jars, "fileUris": &job.Files, "archiveUris": &job.Archives} {
		for _, u := range list(field) {
			f, flow := read(u)
			if flow != nil {
				return flow
			}
			*dst = append(*dst, f)
		}
	}
	for _, a := range proc.Args {
		name := strings.ToLower(a.Name)
		if a.Mode != "IN" {
			job.Out = append(job.Out, a.Name)
		}
		if a.Mode == "OUT" {
			continue
		}
		v, flow := s.evaluate("TO_JSON_STRING("+scope[name].lit+")", "STRING")
		if flow != nil {
			return flow
		}
		job.Params[a.Name] = unquoteLiteral(v.lit)
	}
	// The CALL's child job, whose statistics are the Spark job's.
	rec, flow := s.sendStatement("SELECT 1 AS spark_procedure")
	if flow != nil {
		return flow
	}
	_ = rec
	var kid *scriptChild
	if s.children != nil && len(*s.children) > 0 {
		kid = &(*s.children)[len(*s.children)-1]
		kid.text = stmt
		job.SparkJobID = "bqsp-" + strings.ToLower(strings.ReplaceAll(kid.id, "_", "-"))
	} else {
		job.SparkJobID = "bqsp-" + strings.ToLower(strings.ReplaceAll(jobIDFor(s.r.Context()), "_", "-"))
	}
	stats := map[string]any{"sparkJobId": job.SparkJobID, "sparkJobLocation": job.Location,
		"loggingInfo": map[string]any{"projectId": project, "resourceType": sparkResourceType}}
	if kid != nil {
		kid.spark = stats
	}
	res, err := s.f.spark.Run(s.r.Context(), job)
	if err != nil {
		var u *sparkproc.Unavailable
		if errors.As(err, &u) {
			return fail(http.StatusServiceUnavailable, "backendError", "The Spark runner could not run the procedure: "+u.Msg)
		}
		return fail(http.StatusInternalServerError, "internalError", "cloudburrow: the Spark runner: "+err.Error())
	}
	if res.Failed {
		return fail(http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Spark job %s failed: %s. See the job's logs in Cloud Logging: "+
			`resource.type="%s" resource.labels.spark_job_id="%s"`, job.SparkJobID, res.Message, sparkResourceType, job.SparkJobID))
	}
	names := make([]string, 0, len(outs))
	for name := range outs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var typ string
		var orig string
		for _, a := range proc.Args {
			if strings.EqualFold(a.Name, name) {
				typ, orig = a.Type, a.Name
			}
		}
		raw, ok := res.Out[orig]
		if !ok {
			if v, ok2 := scope[name]; ok2 && v.lit != "" {
				if flow := s.assign(outs[name], v, nil, stmt); flow != nil {
					return flow
				}
			}
			continue
		}
		v, flow := s.sparkOutValue(raw, typ)
		if flow != nil {
			return flow
		}
		if flow := s.assign(outs[name], v, nil, stmt); flow != nil {
			return flow
		}
	}
	return nil
}

// sparkOutValue is an OUT argument's JSON value as a value of typ:
// "TIME and DATETIME values are returned in UTC".
func (s *scriptRun) sparkOutValue(raw, typ string) (scriptVar, *scriptFlow) {
	if strings.TrimSpace(raw) == "null" {
		return scriptVar{typ, "CAST(NULL AS " + typ + ")"}, nil
	}
	upper := strings.ToUpper(strings.TrimSpace(typ))
	expr := "JSON_VALUE(" + sqlString(raw) + ")"
	switch {
	case strings.HasPrefix(upper, "ARRAY") || strings.HasPrefix(upper, "STRUCT"):
		s.notImpl = "Not implemented here: an OUT argument of type " + typ + " of a stored procedure for Apache Spark. Nothing more was run."
		return scriptVar{}, &scriptFlow{}
	case upper == "JSON":
		expr = "PARSE_JSON(" + sqlString(raw) + ")"
	}
	return s.evaluate(expr, typ)
}

// loneSparkProc is sql's one statement when it is a CREATE PROCEDURE of a
// procedure for Apache Spark.
func loneSparkProc(sql string) ([]token, bool) {
	toks, ok := lex(sql)
	if !ok {
		return nil, false
	}
	var found []token
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		if found != nil || !stmt[0].is("CREATE") {
			return nil, false
		}
		found = stmt
	}
	if found == nil {
		return nil, false
	}
	i := 1
	for i < len(found) && !found[i].is("PROCEDURE") {
		if !found[i].is("OR") && !found[i].is("REPLACE") && !found[i].is("TEMP") && !found[i].is("TEMPORARY") {
			return nil, false
		}
		i++
	}
	return found, i < len(found) && isSparkHeader(found)
}

// dryRunSparkProc answers the dry run of stmt, q's lone CREATE PROCEDURE
// of a procedure for Apache Spark: checked as CREATE PROCEDURE checks it,
// and nothing made. The page validates a procedure's properties so: "You
// can use the BigQuery dry run feature to validate your stored procedure
// without creating it."
func (f front) dryRunSparkProc(w http.ResponseWriter, r *http.Request, q queryOptions, stmt []token, insert bool) {
	s := &scriptRun{f: f, r: r, q: q, sql: q.Query, scopes: []map[string]scriptVar{{}}, start: scriptNow(), plain: true, dry: true}
	n := scriptNode{kind: nodeCreateProc, toks: stmt, spark: true, procSpan: [2]int{stmt[0].pos, stmt[len(stmt)-1].end}}
	flow := s.createSparkProc(n)
	if s.notImpl != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", s.notImpl)
		return
	}
	if flow != nil && flow.failed {
		writeRaw(w, flow.status, flow.body)
		return
	}
	if !setQueryText(r, insert, "SELECT 1") {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	if rec.status == http.StatusOK {
		b := dryDDLAnswer(rec.body.Bytes(), "CREATE_PROCEDURE")
		rec.body.Reset()
		rec.body.Write(b)
	}
	f.clientText(w, rec, jobText{query: q.Query})
}
