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
// EXTERNAL SECURITY INVOKER: "The procedure to be executed with the
// privileges of the user that calls it" (the DDL reference), or of the
// custom service account the caller set first with SET
// @@spark_proc_properties.service_account (the page's "Use a custom service
// account"). The job's BigQuery connector then sends the caller's own
// token (one the instance minted: the front's IAM check, iamcheck.go,
// holds it to the caller's permissions), or a token the runner mints for
// that service account, so the job's reads and writes are checked as that
// identity's; a DEFINER procedure (the default) runs as its connection's
// service account. The routine's securityMode is INVOKER.
// @@spark_proc_properties also takes staging_dataset_id (the connector's
// materializationDataset), and staging_bucket and kms_key_name
// (sparkstaging.go).
//
// container_image: an Artifact Registry image of the instance's own
// registry, run with the runtime's Spark and Java mounted into it as
// Google's custom containers guide documents (internal/sparkproc's
// customimage.go). runtime_version: 1.2, 2.2 (the default) and 2.3 run
// Spark 3.5, 3.0 Spark 4.0 and 4.0 Spark 4.2, as Managed Service for
// Apache Spark's runtime pages list them, each on its pinned apache/spark
// image and BigQuery connector (cmd/cloudburrow/sparkrunner.go).
//
// OUT and INOUT values of every type the page's table maps (ARRAY and
// STRUCT among them, a list and a dict); "the built-in library supports
// most of the BigQuery data types except INTERVAL, GEOGRAPHY, NUMERIC, and
// BIGNUMERIC", so an argument of those types is refused when it is made.

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
	// Invoker is EXTERNAL SECURITY INVOKER.
	Invoker bool `json:"invoker,omitempty"`
}

const (
	permConnectionsDelegate = "bigquery.connections.delegate"
	sparkResourceType       = "bigquery.googleapis.com/SparkJob"
)

// sparkRuntimes are the runtime versions Managed Service for Apache Spark
// supports (https://docs.cloud.google.com/managed-spark/docs/concepts/versions/serverless-versions,
// re-read 2026-11-21), every one served.
var sparkRuntimes = map[string]bool{"1.2": true, "2.2": true, "2.3": true, "3.0": true, "4.0": true}

// sparkProcProperties are the fields of @@spark_proc_properties the page
// sets before a CALL.
var sparkProcProperties = map[string]bool{"service_account": true, "staging_bucket": true, "staging_dataset_id": true, "kms_key_name": true}

// sparkUnsupportedTypes are the types the built-in library does not map.
var sparkUnsupportedTypes = []string{"INTERVAL", "GEOGRAPHY", "NUMERIC", "BIGNUMERIC"}

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
	if img := opt("container_image"); img != "" {
		host, rest, _ := strings.Cut(img, "/")
		if !strings.HasSuffix(host, "-docker.pkg.dev") || strings.Count(rest, "/") < 2 {
			return fail("Invalid container_image " + img + ": it is the path of an image in Artifact Registry, " +
				"LOCATION-docker.pkg.dev/PROJECT/REPOSITORY/IMAGE")
		}
	}
	for _, a := range h.args {
		if bad := sparkUnsupportedType(a.Type); bad != "" {
			return fail("Argument " + a.Name + " has type " + a.Type + ": stored procedures for Apache Spark do not support the " + bad +
				" type (the built-in library supports most of the BigQuery data types except INTERVAL, GEOGRAPHY, NUMERIC, and BIGNUMERIC)")
		}
	}
	switch rv := opt("runtime_version"); {
	case rv != "" && !sparkRuntimes[sparkRuntimeMinor(rv)]:
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
	for name, field := range map[string]string{"runtime_version": "runtimeVersion", "main_file_uri": "mainFileUri", "main_class": "mainClass",
		"container_image": "containerImage"} {
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
	if h.invoker {
		routine["securityMode"] = "INVOKER"
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
		Spark: &sparkDef{Language: lang, Code: h.code, Connection: h.connection, Options: spark, Properties: props, Invoker: h.invoker}})
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
		Language: def.Language, Code: def.Code, Properties: def.Properties, ServiceAccount: sa, Params: map[string]string{},
		Types: map[string]string{}}
	job.RuntimeVersion, _ = def.Options["runtimeVersion"].(string)
	job.RuntimeVersion = sparkRuntimeMinor(job.RuntimeVersion)
	job.ContainerImage, _ = def.Options["containerImage"].(string)
	if def.Invoker {
		// The caller's privileges: the custom service account it set,
		// else its own token when the instance minted it (an unminted
		// caller is the instance's own account, as everywhere).
		switch custom := s.sparkProps["service_account"]; {
		case custom != "":
			job.RunAs, job.ServiceAccount = custom, custom
		case mintedCaller(s.r.Context()) != "":
			job.ServiceAccount = mintedCaller(s.r.Context())
			job.AccessToken = strings.TrimSpace(strings.TrimPrefix(s.r.Header.Get("Authorization"), "Bearer "))
		default:
			job.ServiceAccount = callerIdentity(project)
		}
	}
	stage, flow := s.sparkStaging(def, cn, job.ServiceAccount, stmt) // sparkstaging.go
	if flow != nil {
		return flow
	}
	if stage.key != "" {
		job.KMSKey, job.KeyAgent = stage.key, serviceAccountEmail(project, s.f.numbers)
	}
	if d := s.sparkProps["staging_dataset_id"]; d != "" {
		props := map[string]string{"spark.datasource.bigquery.materializationDataset": d}
		for k, v := range job.Properties {
			props[k] = v
		}
		job.Properties = props
	}
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
	for _, a := range proc.Args {
		job.Types[a.Name] = a.Type
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
	run := func(job sparkproc.Job) (sparkproc.Result, *scriptFlow) {
		res, err := s.f.spark.Run(s.r.Context(), job)
		if err != nil {
			var u *sparkproc.Unavailable
			if errors.As(err, &u) {
				return res, fail(http.StatusServiceUnavailable, "backendError", "The Spark runner could not run the procedure: "+u.Msg)
			}
			return res, fail(http.StatusInternalServerError, "internalError", "cloudburrow: the Spark runner: "+err.Error())
		}
		if res.KeyRefused == "" {
			return res, nil
		}
		code, reason := http.StatusBadRequest, "invalid"
		switch res.KeyKind {
		case "permissionDenied":
			code, reason = http.StatusForbidden, "accessDenied"
		case "unavailable":
			code, reason = http.StatusServiceUnavailable, "backendError"
		}
		return res, fail(code, reason, "The customer-managed encryption key "+job.KMSKey+" cannot be used: "+res.KeyRefused)
	}
	// The key is checked before anything is staged with it.
	if job.KMSKey != "" {
		check := job
		check.KeyOnly = true
		if _, flow := run(check); flow != nil {
			return flow
		}
	}
	if err := s.stageSparkFiles(s.r.Context(), stage, job); err != nil {
		return fail(http.StatusBadRequest, "invalid", "The staging bucket "+stage.bucket+" cannot hold the job's files: "+err.Error())
	}
	res, flow := run(job)
	if flow != nil {
		return flow
	}
	if err := s.stageDriverOutput(s.r.Context(), stage, job, res.DriverOutput); err != nil {
		return fail(http.StatusBadRequest, "invalid", "The staging bucket "+stage.bucket+" cannot hold the driver's output: "+err.Error())
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
// "TIME and DATETIME values are returned in UTC" (the runner's library
// writes them so).
func (s *scriptRun) sparkOutValue(raw, typ string) (scriptVar, *scriptFlow) {
	expr, err := sparkOutExpr(raw, typ)
	if err != nil {
		return scriptVar{}, failWith(http.StatusBadRequest, "invalidQuery", "The OUT argument of type "+typ+
			" of the stored procedure for Apache Spark cannot be read: "+err.Error(), "")
	}
	return s.evaluate(expr, typ)
}

// sparkOutExpr is the GoogleSQL expression of raw, a JSON value, as a
// value of typ: an ARRAY a list, a STRUCT a dict (or a list, by field
// position), BYTES base64, the times ISO 8601 strings.
func sparkOutExpr(raw, typ string) (string, error) {
	toks, ok := lex(typ)
	if !ok {
		return "", fmt.Errorf("the type %s cannot be read", typ)
	}
	t, _ := sqlTypeAt(toks, 0)
	if t == nil {
		return "", fmt.Errorf("the type %s cannot be read", typ)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	return sparkValueExpr(v, t)
}

// sparkTypeText is a parsed type's GoogleSQL text.
func sparkTypeText(t map[string]any) string {
	kind, _ := t["typeKind"].(string)
	switch kind {
	case "ARRAY":
		e, _ := t["arrayElementType"].(map[string]any)
		return "ARRAY<" + sparkTypeText(e) + ">"
	case "STRUCT":
		var parts []string
		for _, f := range sparkFields(t) {
			ft, _ := f["type"].(map[string]any)
			n, _ := f["name"].(string)
			if n != "" {
				n = "`" + n + "` "
			}
			parts = append(parts, n+sparkTypeText(ft))
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	case "RANGE":
		e, _ := t["rangeElementType"].(map[string]any)
		return "RANGE<" + sparkTypeText(e) + ">"
	}
	return kind
}

// sparkFields are a STRUCT type's fields.
func sparkFields(t map[string]any) []map[string]any {
	st, _ := t["structType"].(map[string]any)
	switch fs := st["fields"].(type) {
	case []map[string]any:
		return fs
	case []any:
		var out []map[string]any
		for _, x := range fs {
			if f, ok := x.(map[string]any); ok {
				out = append(out, f)
			}
		}
		return out
	}
	return nil
}

// sparkValueExpr is v as an expression of type t.
func sparkValueExpr(v any, t map[string]any) (string, error) {
	text := sparkTypeText(t)
	if v == nil {
		return "CAST(NULL AS " + text + ")", nil
	}
	kind, _ := t["typeKind"].(string)
	switch kind {
	case "ARRAY":
		list, ok := v.([]any)
		if !ok {
			return "", fmt.Errorf("an ARRAY value is a list, not %s", sparkJSONKind(v))
		}
		e, _ := t["arrayElementType"].(map[string]any)
		parts := make([]string, 0, len(list))
		for _, x := range list {
			p, err := sparkValueExpr(x, e)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return text + "[" + strings.Join(parts, ", ") + "]", nil
	case "STRUCT":
		fields := sparkFields(t)
		parts := make([]string, 0, len(fields))
		for i, f := range fields {
			ft, _ := f["type"].(map[string]any)
			n, _ := f["name"].(string)
			var x any
			switch val := v.(type) {
			case map[string]any:
				x = val[n]
				if x == nil {
					for k, y := range val {
						if strings.EqualFold(k, n) {
							x = y
						}
					}
				}
			case []any:
				if i < len(val) {
					x = val[i]
				}
			default:
				return "", fmt.Errorf("a STRUCT value is a dict, not %s", sparkJSONKind(v))
			}
			p, err := sparkValueExpr(x, ft)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return text + "(" + strings.Join(parts, ", ") + ")", nil
	case "JSON":
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return "PARSE_JSON(" + sqlString(string(b)) + ")", nil
	case "BOOL":
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("a BOOL value is true or false, not %s", sparkJSONKind(v))
		}
		return strings.ToUpper(strconv.FormatBool(b)), nil
	case "BYTES":
		str, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("a BYTES value is bytes, not %s", sparkJSONKind(v))
		}
		return "FROM_BASE64(" + sqlString(str) + ")", nil
	case "STRING":
		if str, ok := v.(string); ok {
			return sqlString(str), nil
		}
	}
	switch val := v.(type) {
	case json.Number:
		return "CAST(" + sqlString(val.String()) + " AS " + text + ")", nil
	case string:
		return "CAST(" + sqlString(val) + " AS " + text + ")", nil
	case bool:
		return "CAST(" + strings.ToUpper(strconv.FormatBool(val)) + " AS " + text + ")", nil
	}
	return "", fmt.Errorf("a %s value is not %s", text, sparkJSONKind(v))
}

// sparkJSONKind names a decoded JSON value's kind.
func sparkJSONKind(v any) string {
	switch v.(type) {
	case []any:
		return "a list"
	case map[string]any:
		return "a dict"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a bool"
	}
	return "null"
}

// sparkUnsupportedType is the first of the types the built-in library
// does not map that typ is or holds, or "".
func sparkUnsupportedType(typ string) string {
	toks, ok := lex(typ)
	if !ok {
		return ""
	}
	for _, x := range toks {
		if x.kind != tokWord {
			continue
		}
		u := strings.ToUpper(x.text)
		if alias, ok := typeAliases[u]; ok {
			u = alias
		}
		for _, bad := range sparkUnsupportedTypes {
			if u == bad {
				return bad
			}
		}
	}
	return ""
}

// setSparkProcProperty carries out SET @@spark_proc_properties.<field> =
// expr, which the page runs before a CALL.
func (s *scriptRun) setSparkProcProperty(toks []token) *scriptFlow {
	body := toks[1:]
	stmt := s.text(toks)
	if len(body) < 7 || !body[1].punct("@") || !body[2].is("spark_proc_properties") || !body[3].punct(".") || !body[5].punct("=") {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error: SET @@spark_proc_properties.<field> = <value>", text: stmt}
	}
	field := strings.ToLower(body[4].text)
	if !sparkProcProperties[field] {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Invalid system variable @@spark_proc_properties." + body[4].text +
			": it has the fields service_account, staging_bucket, staging_dataset_id and kms_key_name", text: stmt}
	}
	s.place(body[6:])
	v, flow := s.evaluate(s.text(body[6:]), "STRING")
	if flow != nil {
		return flow
	}
	val := ""
	if lit := strings.TrimSpace(v.lit); lit != "" && !strings.HasPrefix(strings.ToUpper(lit), "CAST(NULL") && !strings.EqualFold(lit, "NULL") {
		val = unquoteLiteral(lit)
	}
	if s.sparkProps == nil {
		s.sparkProps = map[string]string{}
	}
	s.sparkProps[field] = val
	return s.assigned()
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
