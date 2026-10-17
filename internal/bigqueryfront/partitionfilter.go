package bigqueryfront

import "strings"

// Whether a query's filter satisfies require_partition_filter (#1456).
//
// BigQuery (https://cloud.google.com/bigquery/docs/querying-partitioned-tables,
// Require a partition filter in queries): "There must be at least one
// predicate that only references a partitioning column": `WHERE
// partition_id = "20221231" AND f = "20221130"` satisfies it, `WHERE
// partition_id = "20221231" OR f = "20221130"` "is not sufficient, and
// will result in an error" ("Cannot query over table ... without a filter
// that can be used for partition elimination"). And, for the filter to
// prune (same page, Prune (limit) partitions): the partitioning column is
// compared with "a constant expression, rather than a dynamic expression"
// (not a subquery, not another column: `ts >= ts2` does not prune), is
// not in arithmetic (`ts + INTERVAL 1 DAY > CURRENT_TIMESTAMP()` does not
// prune, `ts > CURRENT_TIMESTAMP() - INTERVAL 1 DAY` does), and is
// wrapped only in "a supported built-in function": DATE (the page's
// `DATE(datehour)`), DATE_ADD, DATE_DIFF, DATE_SUB, DATE_TRUNC,
// DATETIME_DIFF, TIMESTAMP_ADD, TIMESTAMP_DIFF, TIMESTAMP_SUB,
// TIMESTAMP_TRUNC, EXTRACT of YEAR or DATE, and FORMAT_TIMESTAMP with
// '%F', '%Y-%m-%d' or '%Y%m%d'; "Partition pruning is not supported for
// functions over an integer range partitioned column".
//
// So prunes reads each WHERE clause of a statement as its conjuncts (its
// top-level ANDs, and those of a conjunct in brackets), and is satisfied
// by one that names a partitioning column, names no other column and no
// subquery, and uses the column as above. A name it cannot tell from a
// column (one not a keyword, a function, a date part or a type) is read
// as a column.

// pruneFunctions are the functions that may wrap the partitioning column.
var pruneFunctions = map[string]bool{"DATE": true, "DATE_ADD": true, "DATE_DIFF": true, "DATE_SUB": true,
	"DATE_TRUNC": true, "DATETIME_DIFF": true, "TIMESTAMP_ADD": true, "TIMESTAMP_DIFF": true, "TIMESTAMP_SUB": true,
	"TIMESTAMP_TRUNC": true, "EXTRACT": true, "FORMAT_TIMESTAMP": true}

// constantWords are words of an expression that name no column.
var constantWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`MICROSECOND MILLISECOND SECOND MINUTE HOUR DAY WEEK ISOWEEK MONTH QUARTER YEAR
		ISOYEAR DAYOFWEEK DAYOFYEAR SUNDAY MONDAY TUESDAY WEDNESDAY THURSDAY FRIDAY SATURDAY DATE DATETIME TIME
		TIMESTAMP INT64 INTEGER FLOAT64 NUMERIC BIGNUMERIC DECIMAL BIGDECIMAL STRING BYTES BOOL BOOLEAN JSON
		CURRENT_DATE CURRENT_DATETIME CURRENT_TIME CURRENT_TIMESTAMP SAFE_CAST`) {
		constantWords[w] = true
	}
}

// prunes reports whether stmt has a WHERE clause with a filter on one of
// columns (case-insensitive) that can prune its partitions; integer is
// for an integer-range partitioned table.
func prunes(stmt []token, columns []string, integer bool) bool {
	for i := range stmt {
		if stmt[i].is("WHERE") && anyConjunctPrunes(whereClause(stmt[i+1:]), columns, integer) {
			return true
		}
	}
	return false
}

// whereClause is the WHERE condition t starts with, up to the clause that
// follows it, the bracket that closes around it or the statement's end.
func whereClause(t []token) []token {
	depth := 0
	for i, tk := range t {
		switch {
		case tk.punct("(") || tk.punct("["):
			depth++
		case tk.punct(")") || tk.punct("]"):
			if depth == 0 {
				return t[:i]
			}
			depth--
		case depth == 0 && (tk.punct(";") || tk.is("GROUP") || tk.is("HAVING") || tk.is("QUALIFY") ||
			tk.is("WINDOW") || tk.is("ORDER") || tk.is("LIMIT") || tk.is("UNION") || tk.is("INTERSECT") ||
			tk.is("EXCEPT")):
			return t[:i]
		}
	}
	return t
}

// anyConjunctPrunes splits cond at its top-level ANDs (an AND of a
// BETWEEN is its own) and reports whether a conjunct prunes.
func anyConjunctPrunes(cond []token, columns []string, integer bool) bool {
	depth, between, start := 0, 0, 0
	var parts [][]token
	for i, tk := range cond {
		switch {
		case tk.punct("(") || tk.punct("["):
			depth++
		case tk.punct(")") || tk.punct("]"):
			depth--
		case depth == 0 && tk.is("BETWEEN"):
			between++
		case depth == 0 && tk.is("AND") && between > 0:
			between--
		case depth == 0 && tk.is("AND"):
			parts = append(parts, cond[start:i])
			start = i + 1
		}
	}
	parts = append(parts, cond[start:])
	for _, p := range parts {
		if inner, ok := bracketed(p); ok {
			if anyConjunctPrunes(inner, columns, integer) {
				return true
			}
			continue
		}
		if conjunctPrunes(p, columns, integer) {
			return true
		}
	}
	return false
}

// bracketed is what is inside t when one pair of brackets holds all of t.
func bracketed(t []token) ([]token, bool) {
	if len(t) < 2 || !t[0].punct("(") || !t[len(t)-1].punct(")") {
		return nil, false
	}
	depth := 0
	for i, tk := range t {
		switch {
		case tk.punct("("):
			depth++
		case tk.punct(")"):
			depth--
			if depth == 0 && i != len(t)-1 {
				return nil, false
			}
		}
	}
	return t[1 : len(t)-1], true
}

// conjunctPrunes reports whether c, a predicate with no top-level AND,
// names a partitioning column, no other column and no subquery, and uses
// the column so that it can prune (above).
func conjunctPrunes(c []token, columns []string, integer bool) bool {
	isColumn := func(name string) bool {
		for _, col := range columns {
			if strings.EqualFold(name, col) {
				return true
			}
		}
		return false
	}
	type call struct {
		fn   string
		open int // the index of its "("
	}
	var calls []call // the brackets open at each token: a function's name, or "" for a plain one
	found := false
	for i := 0; i < len(c); i++ {
		tk := c[i]
		switch {
		case tk.punct("(") || tk.punct("["):
			fn := ""
			if tk.punct("(") && i > 0 && c[i-1].kind == tokWord {
				fn = strings.ToUpper(c[i-1].text)
			}
			calls = append(calls, call{fn: fn, open: i})
			continue
		case tk.punct(")") || tk.punct("]"):
			if len(calls) > 0 {
				calls = calls[:len(calls)-1]
			}
			continue
		case tk.punct("@"): // a query parameter or a system variable
			for i+1 < len(c) && (c[i+1].punct("@") || c[i+1].kind == tokWord) {
				i++
				if c[i].kind == tokWord {
					break
				}
			}
			continue
		case tk.kind == tokString || tk.kind == tokPunct:
			continue
		case tk.is("SELECT"):
			return false
		}
		word := tk.kind == tokWord
		if word && tk.text[0] >= '0' && tk.text[0] <= '9' { // a number, 1.5 as three tokens
			for i+2 < len(c) && c[i+1].punct(".") && c[i+2].kind == tokWord {
				i += 2
			}
			continue
		}
		if word && i+1 < len(c) && c[i+1].punct("(") { // a function's name
			continue
		}
		if word && (reservedWords[strings.ToUpper(tk.text)] || constantWords[strings.ToUpper(tk.text)]) &&
			(!isColumn(tk.text) || i+1 < len(c) && c[i+1].kind == tokString ||
				len(calls) > 0 && calls[len(calls)-1].open == i-1 && calls[len(calls)-1].fn == "EXTRACT") {
			continue // a keyword, a date part or a type; a column of such a name is read as the column
		}
		// A column, maybe qualified: its last name.
		last := i
		for last+2 < len(c) && c[last+1].punct(".") && (c[last+2].kind == tokWord || c[last+2].kind == tokQuoted) {
			last += 2
		}
		name := c[last].text
		start := i
		i = last
		if !isColumn(name) {
			return false
		}
		if start > 0 && arithmetic(c[start-1]) || last+1 < len(c) && arithmetic(c[last+1]) {
			return false
		}
		for _, in := range calls { // each function around the column
			switch {
			case in.fn == "":
			case integer || !pruneFunctions[in.fn]:
				return false
			case in.fn == "EXTRACT":
				if part := c[in.open+1]; !part.is("YEAR") && !part.is("DATE") {
					return false
				}
			case in.fn == "FORMAT_TIMESTAMP":
				if f := c[in.open+1]; f.kind != tokString || !prunableFormat(f.text) {
					return false
				}
			}
		}
		found = true
	}
	return found
}

// arithmetic reports whether t is an arithmetic or concatenation operator.
func arithmetic(t token) bool {
	return t.punct("+") || t.punct("-") || t.punct("*") || t.punct("/") || t.punct("|")
}

// prunableFormat reports whether lit, a string literal as written, is a
// format FORMAT_TIMESTAMP of the partitioning column prunes with.
func prunableFormat(lit string) bool {
	s := strings.Trim(lit, `'"`)
	return s == "%F" || s == "%Y-%m-%d" || s == "%Y%m%d"
}
