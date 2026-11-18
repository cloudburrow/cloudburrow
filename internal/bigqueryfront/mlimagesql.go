package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// How the image functions (mlimage.go, #1798) run: each outermost call
// of ML.DECODE_IMAGE, ML.RESIZE_IMAGE, ML.CONVERT_COLOR_SPACE or
// ML.CONVERT_IMAGE_TYPE is evaluated in the front before the engine runs
// the query, as the scalar AI functions are (aigenerate.go): the distinct
// values of its arguments over its query block's FROM clause are read
// (TO_JSON_STRING of a key), each is computed here, and the call is
// written as a lookup in a literal table of the answers. A row whose
// image fails (not an image, over 10 MB, a bad STRUCT) holds its error,
// raised with ERROR() only where the query reaches that row, so a WHERE
// that filters an object out never fails on it.
//
// The data pseudocolumn: ML.DECODE_IMAGE(data), or a qualified
// t.data / dataset.table.data, of an object table the block reads, is the
// object's bytes, read at its row's generation through the table's
// connection (objectData, objecttables.go); its key is the row's uri and
// generation. data anywhere else is still the unknown name it is to the
// engine ("You can't include the data pseudocolumn in queries"). Any other
// argument is a BYTES expression (its key TO_BASE64 of it).

// imageFns are the image functions, by name.
var imageFns = map[string]bool{"DECODE_IMAGE": true, "RESIZE_IMAGE": true, "CONVERT_COLOR_SPACE": true, "CONVERT_IMAGE_TYPE": true}

// imageCall is one image function call.
type imageCall struct {
	fn         string
	start, end int // byte offsets of ML ... )
	args       []string
	inner      *imageCall // the image argument, when it is an image call
	block      int        // the block's SELECT token
	// ML.DECODE_IMAGE of the data pseudocolumn: the qualifier written
	// before data ("" or "t." ...), and the object table.
	data  bool
	qual  string
	table objectTable
	// ints is the image argument's form (INT64 values), when it is not a
	// call (read from the engine's schema of it).
	ints bool
}

func (c *imageCall) name() string { return "ML." + c.fn }

// outInts is the form of c's result.
func (c *imageCall) outInts() bool {
	switch c.fn {
	case "DECODE_IMAGE", "CONVERT_COLOR_SPACE":
		return false
	case "CONVERT_IMAGE_TYPE":
		return true
	}
	if c.inner != nil {
		return c.inner.outInts()
	}
	return c.ints
}

// key is the SQL expression c's rows are told apart by.
func (c *imageCall) key() string {
	if c.fn == "DECODE_IMAGE" {
		if c.data {
			return "STRUCT(" + c.qual + "uri AS u, " + c.qual + "generation AS g)"
		}
		return "TO_BASE64(" + c.args[0] + ")"
	}
	img := c.args[0]
	if c.inner != nil {
		img = c.inner.key()
	}
	fs := []string{img + " AS a"}
	for i, a := range c.args[1:] {
		fs = append(fs, a+" AS b"+strconv.Itoa(i+1))
	}
	return "STRUCT(" + strings.Join(fs, ", ") + ")"
}

// imageArity is each function's argument count.
var imageArity = map[string]int{"DECODE_IMAGE": 1, "RESIZE_IMAGE": 4, "CONVERT_COLOR_SPACE": 2, "CONVERT_IMAGE_TYPE": 1}

// parseImageCall reads the call whose ML token is toks[i].
func parseImageCall(sql string, toks []token, depth []int, i int) (*imageCall, int, int, string) {
	fn := strings.ToUpper(toks[i+2].text)
	end := matchParen(toks, i+3)
	if end < 0 {
		return nil, 0, 400, "Syntax error: unclosed ML." + fn + "("
	}
	c := &imageCall{fn: fn, start: toks[i].pos, end: toks[end].end, block: -1}
	var argToks [][]token
	k0 := i + 4
	brackets := 0
	for k := k0; k <= end; k++ {
		switch {
		case toks[k].punct("["):
			brackets++
		case toks[k].punct("]"):
			brackets--
		case toks[k].punct("<") && (toks[k-1].is("ARRAY") || toks[k-1].is("STRUCT")):
			brackets++
		case toks[k].punct(">") && brackets > 0 && !toks[k-1].punct("="):
			brackets--
		}
		if k == end || toks[k].punct(",") && depth[k] == depth[i+3]+1 && brackets == 0 {
			if k > k0 {
				argToks = append(argToks, toks[k0:k])
			}
			k0 = k + 1
		}
	}
	if len(argToks) != imageArity[fn] {
		return nil, 0, 400, fmt.Sprintf("No matching signature for function ML.%s: it takes %d argument(s), not %d", fn, imageArity[fn], len(argToks))
	}
	for _, a := range argToks {
		c.args = append(c.args, strings.TrimSpace(sql[a[0].pos:a[len(a)-1].end]))
	}
	first := argToks[0]
	if fn != "DECODE_IMAGE" && len(first) > 4 && first[0].is("ML") && first[1].punct(".") && imageFns[strings.ToUpper(first[2].text)] && first[3].punct("(") {
		j := indexOfToken(toks, first[0])
		inner, innerEnd, code, msg := parseImageCall(sql, toks, depth, j)
		if code != 0 {
			return nil, 0, code, msg
		}
		if toks[innerEnd].end == first[len(first)-1].end {
			c.inner = inner
		}
	}
	if fn == "DECODE_IMAGE" {
		// data, t.data, dataset.table.data, `dataset.table`.data
		a := first
		if n := len(a); n >= 1 && a[n-1].kind == tokWord && strings.EqualFold(a[n-1].text, "data") {
			ok := true
			for k := 0; k < n-1; k++ {
				if k%2 == 1 && !a[k].punct(".") || k%2 == 0 && a[k].kind != tokWord && a[k].kind != tokQuoted {
					ok = false
				}
			}
			if ok && n%2 == 1 {
				c.data = true
				c.qual = sql[a[0].pos:a[n-1].pos]
				if n == 1 {
					c.qual = ""
				}
			}
		}
	}
	return c, end, 0, ""
}

func indexOfToken(toks []token, t token) int {
	for i := range toks {
		if toks[i].pos == t.pos {
			return i
		}
	}
	return -1
}

// findImageCalls finds sql's outermost image function calls.
func findImageCalls(sql string, toks []token, depth []int) ([]*imageCall, int, string) {
	var calls []*imageCall
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("ML") || !toks[i+1].punct(".") || toks[i+2].kind != tokWord || !toks[i+3].punct("(") || i > 0 && toks[i-1].punct(".") ||
			!imageFns[strings.ToUpper(toks[i+2].text)] {
			continue
		}
		c, end, code, msg := parseImageCall(sql, toks, depth, i)
		if code != 0 {
			return nil, code, msg
		}
		m := depth[i]
		for k := i - 1; k >= 0; k-- {
			m = min(m, depth[k])
			if toks[k].is("SELECT") && depth[k] == m {
				c.block = k
				break
			}
		}
		if c.block < 0 {
			return nil, 501, "Not implemented here: " + c.name() + " outside a SELECT query block. Nothing was run."
		}
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
}

// rewriteImageFunctions evaluates text's image function calls and writes
// their answers into it; text is returned as it was when it has none.
func (f front) rewriteImageFunctions(r *http.Request, q queryOptions, text string) (string, int, string) {
	u := strings.ToUpper(text)
	if !strings.Contains(u, "_IMAGE") && !strings.Contains(u, "CONVERT_COLOR_SPACE") {
		return text, 0, ""
	}
	toks, ok := lex(text)
	if !ok {
		return text, 0, ""
	}
	depth := tokenDepths(toks)
	calls, code, msg := findImageCalls(text, toks, depth)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	if f.objects != nil {
		if code, msg := f.objectTablesRead(r, q); code != 0 {
			return "", code, msg
		}
	}
	var out strings.Builder
	last := 0
	for _, c := range calls {
		src := blockSource(text, toks, depth, c.block)
		if code, msg := f.resolveImageCall(r, q, c, src); code != 0 {
			return "", code, msg
		}
		probe := fmt.Sprintf(src, c.key())
		if strings.Contains(probe, text[c.start:c.end]) {
			return "", 501, "Not implemented here: " + c.name() + " inside a FROM clause (such as a JOIN's ON condition), whose images the front cannot read " +
				"before the join runs. Nothing was run."
		}
		_, rows, code, msg := f.queryAllRows(r, q, probe)
		if code != 0 {
			return "", code, msg
		}
		ev := imageEval{f: f, r: r, objects: map[string]imageResult{}}
		var lits []string
		for _, row := range rows {
			if len(row) != 1 || row[0] == nil {
				continue
			}
			m, why := ev.eval(c, json.RawMessage(*row[0]))
			e, v := "CAST(NULL AS STRING)", "NULL"
			switch {
			case why != "":
				e = sqlString(why)
			case m != nil:
				v = m.literal()
			}
			lits = append(lits, "STRUCT("+sqlString(*row[0])+", "+e+", "+v+")")
		}
		// The STRUCT's fields have no names; a stored or returned one's
		// are BigQuery's names of unnamed fields, _field_1 and _field_2.
		vt := "STRUCT<_field_1 ARRAY<INT64>, _field_2 ARRAY<FLOAT64>>"
		if c.outInts() {
			vt = "STRUCT<_field_1 ARRAY<INT64>, _field_2 ARRAY<INT64>>"
		}
		out.WriteString(text[last:c.start])
		out.WriteString("(SELECT IF(_cbimg.e IS NULL, _cbimg.v, ERROR(_cbimg.e)) FROM UNNEST(ARRAY<STRUCT<k STRING, e STRING, v " + vt + ">>[" +
			strings.Join(lits, ", ") + "]) AS _cbimg WHERE _cbimg.k = TO_JSON_STRING(" + c.key() + "))")
		last = c.end
	}
	out.WriteString(text[last:])
	return out.String(), 0, ""
}

// resolveImageCall finds the object table of c's data pseudocolumn, and
// the form of an image argument that is not a call, recursively.
func (f front) resolveImageCall(r *http.Request, q queryOptions, c *imageCall, src string) (int, string) {
	if c.inner != nil {
		return f.resolveImageCall(r, q, c.inner, src)
	}
	if c.fn != "DECODE_IMAGE" {
		// The image argument's type, from the engine's schema of it.
		probe := strings.Replace(src, "SELECT DISTINCT TO_JSON_STRING(%s) AS _cbai_k", "SELECT %s AS _cbimg_t", 1)
		probe = strings.Replace(probe, "SELECT TO_JSON_STRING(%s) AS _cbai_k", "SELECT %s AS _cbimg_t", 1)
		fields, code, msg := f.querySchema(r, q, fmt.Sprintf(probe, c.args[0])+" LIMIT 0")
		if code != 0 {
			return code, msg
		}
		bad := 400
		if len(fields) == 1 && fields[0].Type == "RECORD" && len(fields[0].Fields) == 2 && fields[0].Fields[0].Type == "INTEGER" &&
			fields[0].Fields[0].Mode == "REPEATED" && fields[0].Fields[1].Mode == "REPEATED" {
			switch fields[0].Fields[1].Type {
			case "INTEGER":
				c.ints, bad = true, 0
			case "FLOAT":
				bad = 0
			}
		}
		if bad != 0 {
			return 400, "No matching signature for function " + c.name() + ": the image argument must be a STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>> or STRUCT<ARRAY<INT64>, ARRAY<INT64>>"
		}
		return 0, ""
	}
	if !c.data || f.objects == nil {
		c.data = false
		return 0, ""
	}
	low := strings.ToLower(src)
	project := projectOf(f.base)
	var found []objectTable
	f.objects.mu.Lock()
	for _, ot := range f.objects.m {
		if projectOf(ot.Base) == project && strings.Contains(low, strings.ToLower(ot.Table)) && (strings.Contains(low, strings.ToLower(ot.Dataset)) ||
			strings.Contains(strings.ToLower(string(q.DefaultDataset)), `"`+strings.ToLower(ot.Dataset)+`"`)) {
			found = append(found, ot)
		}
	}
	f.objects.mu.Unlock()
	var live []objectTable
	for _, ot := range found {
		if _, ok := f.objectTable(r, ot.Dataset, ot.Table); ok {
			live = append(live, ot)
		}
	}
	switch {
	case len(live) == 0:
		// A column named data of an ordinary table: BYTES.
		c.data = false
		return 0, ""
	case len(live) == 1:
		c.table = live[0]
		return 0, ""
	}
	qual := strings.ToLower(strings.Trim(strings.TrimSuffix(strings.TrimSpace(c.qual), "."), "`"))
	for _, ot := range live {
		t := strings.ToLower(ot.Table)
		if qual == t || strings.HasSuffix(qual, "."+t) {
			c.table = ot
			return 0, ""
		}
		if qual != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(ot.Table)+"`?\\s+(as\\s+)?`?"+regexp.QuoteMeta(qual)+`\b`).MatchString(src) {
			c.table = ot
			return 0, ""
		}
	}
	return 400, "Column name data is ambiguous: the query reads more than one object table; qualify it as dataset_name.object_table_name.data"
}

// schemaField is a query result's schema field.
type schemaField struct {
	Name   string        `json:"name"`
	Type   string        `json:"type"`
	Mode   string        `json:"mode"`
	Fields []schemaField `json:"fields"`
}

// querySchema is sql's result schema, nested fields and all.
func (f front) querySchema(r *http.Request, q queryOptions, sql string) ([]schemaField, int, string) {
	text, _, msg := qualifyTables(sql, defaultDatasetOf(q))
	if msg != "" {
		return nil, 400, msg
	}
	req, err := json.Marshal(map[string]any{"query": text, "useLegacySql": false, "defaultDataset": q.DefaultDataset,
		"parameterMode": q.ParameterMode, "queryParameters": q.QueryParameters, "maxResults": 0, "timeoutMs": 600000})
	if err != nil {
		return nil, 500, err.Error()
	}
	st, got := f.send(r, http.MethodPost, "/queries", req)
	var p struct {
		Schema struct {
			Fields []schemaField `json:"fields"`
		} `json:"schema"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(got, &p) != nil || st != http.StatusOK {
		if st >= 500 || st == 0 {
			return nil, 500, "cloudburrow: could not read the image argument's type: " + p.Error.Message
		}
		return nil, st, p.Error.Message
	}
	return p.Schema.Fields, 0, ""
}

// imageResult is an object's decoded image, or why it is not one.
type imageResult struct {
	m   *mlImage
	why string
}

// imageEval computes calls' images from their keys' values.
type imageEval struct {
	f       front
	r       *http.Request
	objects map[string]imageResult
}

// eval is c of the key value raw: the image (nil for NULL) or the row's
// error.
func (ev imageEval) eval(c *imageCall, raw json.RawMessage) (*mlImage, string) {
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" {
		return nil, ""
	}
	if c.fn == "DECODE_IMAGE" {
		if c.data {
			var k struct {
				U string      `json:"u"`
				G json.Number `json:"g"`
			}
			if err := json.Unmarshal(raw, &k); err != nil || k.U == "" {
				return nil, ""
			}
			key := k.U + "#" + k.G.String()
			if res, ok := ev.objects[key]; ok {
				return res.m, res.why
			}
			gen, _ := strconv.ParseInt(strings.Trim(k.G.String(), `"`), 10, 64)
			b, code, msg := ev.f.objectData(ev.r, c.table, k.U, gen)
			var res imageResult
			if code != 0 {
				res.why = "ML.DECODE_IMAGE: " + msg
			} else {
				res.m, res.why = decodeImage(b, k.U)
			}
			ev.objects[key] = res
			return res.m, res.why
		}
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil || s == nil {
			return nil, ""
		}
		b, err := base64.StdEncoding.DecodeString(*s)
		if err != nil {
			return nil, "ML.DECODE_IMAGE: the data argument must be BYTES"
		}
		return decodeImage(b, "the data argument")
	}
	var k map[string]json.RawMessage
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, "cloudburrow: unreadable image arguments"
	}
	var img *mlImage
	var why string
	if c.inner != nil {
		img, why = ev.eval(c.inner, k["a"])
	} else {
		img, why = imageFromJSON(k["a"], c.ints, c.name())
	}
	if why != "" || img == nil {
		return nil, why
	}
	switch c.fn {
	case "CONVERT_IMAGE_TYPE":
		return convertImageType(img)
	case "CONVERT_COLOR_SPACE":
		var space *string
		if json.Unmarshal(k["b1"], &space) != nil {
			return nil, "No matching signature for function ML.CONVERT_COLOR_SPACE: target_color_space must be a STRING"
		}
		if space == nil {
			return nil, ""
		}
		return convertColorSpace(img, *space)
	}
	// RESIZE_IMAGE
	var h, w *int64
	var p *bool
	if json.Unmarshal(k["b1"], &h) != nil || json.Unmarshal(k["b2"], &w) != nil {
		return nil, "No matching signature for function ML.RESIZE_IMAGE: target_height and target_width must be INT64"
	}
	if json.Unmarshal(k["b3"], &p) != nil {
		return nil, "No matching signature for function ML.RESIZE_IMAGE: preserve_aspect_ratio must be a BOOL"
	}
	if h == nil || w == nil || p == nil {
		return nil, ""
	}
	if *h <= 0 || *w <= 0 {
		return nil, fmt.Sprintf("ML.RESIZE_IMAGE: target_height and target_width must be positive, not %d and %d", *h, *w)
	}
	th, tw := resizeTo(img, int(*h), int(*w), *p)
	if 8*(3+th*tw*img.c) > maxImageStructBytes {
		return nil, fmt.Sprintf("ML.RESIZE_IMAGE: a %d × %d image of %d channels is over 60 MB", th, tw, img.c)
	}
	return resizeImage(img, th, tw), ""
}
