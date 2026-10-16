package bigqueryfront

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

// default_time_zone (#1417, projectoptions.go). BigQuery: "the default
// time zone to use in time zone-dependent GoogleSQL functions, when a time
// zone is not specified as an argument"
// (https://cloud.google.com/bigquery/docs/default-configuration). The
// engine's default zone is UTC (googlesqlite patch 0022), and it builds no
// other, so the front writes the zone into the query, in each call that
// gives none (rewriteTimeZone), where the call's form shows it is one that
// takes a zone:
//
//   - CURRENT_DATE, CURRENT_DATETIME and CURRENT_TIME, with or without ();
//   - FORMAT_TIMESTAMP(format, ts), PARSE_TIMESTAMP(format, s) and
//     TIMESTAMP_TRUNC(ts, part), given two arguments;
//   - TIMESTAMP(x), given one (every one-argument form takes a zone);
//   - DATE(x), DATETIME(x), TIME(x), STRING(x) and EXTRACT(part FROM x),
//     when x is itself a TIMESTAMP the text shows: a TIMESTAMP literal,
//     CURRENT_TIMESTAMP, a call of TIMESTAMP, TIMESTAMP_SECONDS,
//     TIMESTAMP_MILLIS, TIMESTAMP_MICROS, TIMESTAMP_ADD, TIMESTAMP_SUB,
//     TIMESTAMP_TRUNC or PARSE_TIMESTAMP, or a CAST(... AS TIMESTAMP).
//
// Not rewritten, and so still in UTC (docs/compatibility.md): those of
// DATE, DATETIME, TIME, STRING and EXTRACT whose argument is a column or
// any other expression (the front has no types to tell a TIMESTAMP from a
// DATETIME, where adding a zone would fail), DATE_TRUNC and the others
// that take a TIMESTAMP only in one of their forms, a TIMESTAMP literal
// with no offset, and a cast between TIMESTAMP and STRING, DATE or
// DATETIME. The job keeps the client's text.

// validTimeZone reports whether BigQuery reads s as a time zone: a tz
// database name, or an offset from UTC (+05, -08:00, UTC+3).
func validTimeZone(s string) bool {
	if offsetZone.MatchString(s) {
		return true
	}
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}

var offsetZone = regexp.MustCompile(`^(?i:UTC)?[+-]\d{1,2}(:?\d{2})?$`)

// zoneFirst are the functions given the zone when called with no
// argument; zoneThird those given it after two, zoneSecond after one, and
// zoneOfTimestamp after one that is a TIMESTAMP.
var (
	zoneFirst       = map[string]bool{"CURRENT_DATE": true, "CURRENT_DATETIME": true, "CURRENT_TIME": true}
	zoneThird       = map[string]bool{"FORMAT_TIMESTAMP": true, "PARSE_TIMESTAMP": true, "TIMESTAMP_TRUNC": true}
	zoneOfTimestamp = map[string]bool{"DATE": true, "DATETIME": true, "TIME": true, "STRING": true}
	timestampCalls  = map[string]bool{"CURRENT_TIMESTAMP": true, "TIMESTAMP": true, "TIMESTAMP_SECONDS": true,
		"TIMESTAMP_MILLIS": true, "TIMESTAMP_MICROS": true, "TIMESTAMP_ADD": true, "TIMESTAMP_SUB": true,
		"TIMESTAMP_TRUNC": true, "PARSE_TIMESTAMP": true}
)

// rewriteTimeZone writes zone into each call of sql that takes one and
// gives none (above). ok is false when nothing changed.
func rewriteTimeZone(sql, zone string) (string, bool) {
	toks, lexed := lex(sql)
	if !lexed {
		return sql, false
	}
	lit := sqlString(zone)
	type edit struct {
		pos  int
		text string
	}
	var edits []edit
	for i, t := range toks {
		if t.kind != tokWord || i > 0 && (toks[i-1].punct(".") || toks[i-1].is("AS")) {
			continue
		}
		name := strings.ToUpper(t.text)
		open := i+1 < len(toks) && toks[i+1].punct("(")
		if zoneFirst[name] {
			switch {
			case !open:
				edits = append(edits, edit{t.end, "(" + lit + ")"})
			case i+2 < len(toks) && toks[i+2].punct(")"):
				edits = append(edits, edit{toks[i+2].pos, lit})
			}
			continue
		}
		if !open {
			continue
		}
		args, closing := callArgs(toks, i+1)
		if closing < 0 {
			continue
		}
		switch {
		case zoneThird[name] && len(args) == 2,
			name == "TIMESTAMP" && len(args) == 1,
			zoneOfTimestamp[name] && len(args) == 1 && isTimestampExpr(args[0]):
			edits = append(edits, edit{toks[closing].pos, ", " + lit})
		case name == "EXTRACT" && len(args) == 1:
			a := args[0]
			from := -1
			at := false
			depth := 0
			for j, x := range a {
				switch {
				case x.punct("("):
					depth++
				case x.punct(")"):
					depth--
				case depth == 0 && x.is("FROM") && from < 0:
					from = j
				case depth == 0 && x.is("AT"):
					at = true
				}
			}
			if from >= 0 && !at && isTimestampExpr(a[from+1:]) {
				edits = append(edits, edit{toks[closing].pos, " AT TIME ZONE " + lit})
			}
		}
	}
	if len(edits) == 0 {
		return sql, false
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.pos
	}
	b.WriteString(sql[last:])
	return b.String(), true
}

// callArgs splits the arguments of the call whose "(" is toks[open], at
// its top-level commas; closing is the index of its ")", -1 when there is
// none.
func callArgs(toks []token, open int) (args [][]token, closing int) {
	depth := 0
	start := open + 1
	for j := open; j < len(toks); j++ {
		switch {
		case toks[j].punct("(") || toks[j].punct("["):
			depth++
		case toks[j].punct(")") || toks[j].punct("]"):
			depth--
			if depth == 0 {
				if j > start {
					args = append(args, toks[start:j])
				}
				return args, j
			}
		case depth == 1 && toks[j].punct(","):
			args = append(args, toks[start:j])
			start = j + 1
		}
	}
	return nil, -1
}

// isTimestampExpr reports whether a is, by its text alone, a TIMESTAMP.
func isTimestampExpr(a []token) bool {
	if len(a) == 0 || a[0].kind != tokWord {
		return false
	}
	name := strings.ToUpper(a[0].text)
	switch {
	case name == "TIMESTAMP" && len(a) == 2 && a[1].kind == tokString:
		return true
	case name == "CURRENT_TIMESTAMP" && len(a) == 1:
		return true
	case len(a) >= 3 && a[1].punct("("):
		args, closing := callArgs(a, 1)
		if closing != len(a)-1 {
			return false
		}
		if name == "CAST" || name == "SAFE_CAST" {
			if len(args) != 1 {
				return false
			}
			x := args[0]
			return len(x) >= 3 && x[len(x)-2].is("AS") && x[len(x)-1].is("TIMESTAMP")
		}
		return timestampCalls[name]
	}
	return false
}

// withDefaultTimeZone serves q with the default_time_zone of the job's
// region written into it (above), when one is set, and reports whether it
// did. The job keeps the client's text.
func (f front) withDefaultTimeZone(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.settings == nil || !f.settings.hasAny("default_time_zone") {
		return false
	}
	loc := queryLocation(r, insert)
	if loc == "" {
		loc = f.settings.defaultLocation(projectOf(f.base))
	}
	zone := f.settings.effective(projectOf(f.base), loc, "default_time_zone")
	if zone == "" || strings.EqualFold(zone, "UTC") {
		return false
	}
	sql, changed := rewriteTimeZone(q.Query, zone)
	if !changed {
		return false
	}
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
	return true
}

// hasAny reports whether any project or the organization sets option
// name, in any region.
func (p *projectOptions) hasAny(name string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, opts := range p.opts {
		for k := range opts {
			if k == name || strings.HasSuffix(k, "."+name) {
				return true
			}
		}
	}
	return false
}
