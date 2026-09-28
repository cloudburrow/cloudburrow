package bigqueryfront

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// A CSV load's fieldDelimiter, quote, allowJaggedRows and nullMarker
// (#945). BigQuery honours each:
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad
//
//   - fieldDelimiter: "The separator character for fields in a CSV file.
//     The separator is interpreted as a single byte. ... BigQuery also
//     supports the escape sequence "\t" (U+0009) to specify a tab
//     separator. The default value is comma".
//   - quote: "The value that is used to quote data sections in a CSV
//     file. ... The default value is a double-quote ('"'). If your data
//     does not contain quoted sections, set the property value to an empty
//     string. ... To include the specific quote character within a quoted
//     value, precede it with an additional matching quote character."
//   - allowJaggedRows: "Accept rows that are missing trailing optional
//     columns. The missing values are treated as nulls."
//   - nullMarker: "Specifies a string that represents a null value in a
//     CSV file. ... The default value is the empty string. If you set this
//     property to a custom value, BigQuery throws an error if an empty
//     string is present for all data types except for STRING and BYTE. For
//     STRING and BYTE columns, BigQuery interprets the empty string as an
//     empty value."
//
// The emulator reads a load's data with Go's encoding/csv defaults and
// uses none of them (its source, server/handler.go; measured against the
// pinned image through the official Go client, with a schema): a tab- or
// pipe-delimited file and a file quoted with ' failed, 400 "wrong number
// of fields"; with quote "" it loaded "x" (quoted in the file) as x; a
// short row with allowJaggedRows failed the same way; with nullMarker \N
// it loaded the text \N, and an empty value as NULL. It loads every empty
// value as NULL.
//
// So when a load sets any of them, the front reads the data by BigQuery's
// rules as it passes and writes it on as the CSV the emulator reads:
// comma-separated, quoted with ", every row as wide as the table, a null
// written as an empty value. What the emulator could not load as BigQuery
// would fails the load instead (loadDataError): with a custom nullMarker,
// an empty value, which the emulator would load as NULL (501 for a STRING
// or BYTES column, where BigQuery loads an empty string, and 400 invalid
// for any other, which BigQuery refuses); and a quote that is not closed,
// or data between a closing quote and the next delimiter (400 invalid). A
// fieldDelimiter or quote of more than one byte, or outside ASCII, is 501
// before anything is read.

// csvDialect is how a CSV load's data is read.
type csvDialect struct {
	delim byte
	// quote is 0 for none.
	quote     byte
	jagged    bool
	nullMark  string
	nullIsSet bool
}

// plain reports whether d is how the emulator reads CSV itself.
func (d csvDialect) plain() bool {
	return d.delim == ',' && d.quote == '"' && !d.jagged && !d.nullIsSet
}

// dialectOf returns the dialect a load's options give, or why it is 501.
func dialectOf(fieldDelimiter string, quote *string, jagged bool, nullMarker *string) (csvDialect, string) {
	d := csvDialect{delim: ',', quote: '"', jagged: jagged}
	switch fd := fieldDelimiter; {
	case fd == "":
	case fd == `\t`:
		d.delim = '\t'
	case len(fd) == 1 && fd[0] >= 1 && fd[0] < 0x80 && fd[0] != '\n' && fd[0] != '\r':
		d.delim = fd[0]
	default:
		return d, fmt.Sprintf("fieldDelimiter %q: CloudBurrow reads a delimiter of one ASCII character (or \\t), and "+
			"the emulator behind it none but a comma", fd)
	}
	if quote != nil {
		switch q := *quote; {
		case q == "":
			d.quote = 0
		case len(q) == 1 && q[0] >= 1 && q[0] < 0x80 && q[0] != '\n' && q[0] != '\r':
			d.quote = q[0]
		default:
			return d, fmt.Sprintf("quote %q: CloudBurrow reads a quote of one ASCII character, or none, and the emulator "+
				"behind it none but \"", q)
		}
	}
	if d.quote != 0 && d.quote == d.delim {
		return d, fmt.Sprintf("quote %q is also the fieldDelimiter", string(d.quote))
	}
	if nullMarker != nil && *nullMarker != "" {
		d.nullMark, d.nullIsSet = *nullMarker, true
	}
	return d, ""
}

// loadDataError is why the front failed a load while its data passed
// through it.
type loadDataError struct {
	code   int
	reason string
	msg    string
}

func (e *loadDataError) Error() string { return e.msg }

// dataFailure holds the loadDataError a load's data stream ended with.
type dataFailure struct {
	mu  sync.Mutex
	err *loadDataError
}

func (d *dataFailure) set(e *loadDataError) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = e
	}
}

func (d *dataFailure) get() *loadDataError {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// csvRecords reads CSV records by a dialect.
type csvRecords struct {
	r    *bufio.Reader
	d    csvDialect
	line int
}

// next returns the next record, and for each field whether it was quoted.
// An empty line is not a record, as encoding/csv, which the emulator reads
// the data with, skips it. It returns io.EOF at the end of the data.
func (c *csvRecords) next() ([]string, []bool, error) {
	for {
		rec, quoted, empty, err := c.read()
		if err != nil {
			return nil, nil, err
		}
		if !empty {
			return rec, quoted, nil
		}
	}
}

func (c *csvRecords) read() (rec []string, quoted []bool, empty bool, err error) {
	c.line++
	var f strings.Builder
	inQuote, wasQuoted, afterQuote, seen := false, false, false, false
	end := func() {
		rec = append(rec, f.String())
		quoted = append(quoted, wasQuoted)
		f.Reset()
		wasQuoted, afterQuote = false, false
	}
	for {
		b, rerr := c.r.ReadByte()
		if errors.Is(rerr, io.EOF) {
			if inQuote {
				return nil, nil, false, c.invalid("a quoted value is not closed")
			}
			if !seen {
				return nil, nil, false, io.EOF
			}
			end()
			return rec, quoted, false, nil
		}
		if rerr != nil {
			return nil, nil, false, rerr
		}
		seen = true
		switch {
		case inQuote && b == c.d.quote:
			if nb, err := c.r.Peek(1); err == nil && nb[0] == c.d.quote {
				_, _ = c.r.ReadByte()
				f.WriteByte(b)
				continue
			}
			inQuote, afterQuote = false, true
		case inQuote:
			if b == '\n' {
				c.line++
			}
			f.WriteByte(b)
		case b == c.d.delim:
			end()
		case b == '\n':
			if len(rec) == 0 && f.Len() == 0 && !wasQuoted {
				return nil, nil, true, nil
			}
			end()
			return rec, quoted, false, nil
		case b == '\r':
			if nb, err := c.r.Peek(1); err == nil && nb[0] == '\n' {
				continue
			}
			if afterQuote {
				return nil, nil, false, c.invalid("data between a closing quote and the field delimiter")
			}
			f.WriteByte(b)
		case afterQuote:
			return nil, nil, false, c.invalid("data between a closing quote and the field delimiter")
		case c.d.quote != 0 && b == c.d.quote && f.Len() == 0 && !wasQuoted:
			inQuote, wasQuoted = true, true
		default:
			f.WriteByte(b)
		}
	}
}

func (c *csvRecords) invalid(what string) error {
	return &loadDataError{code: 400, reason: "invalid", msg: fmt.Sprintf("Error while reading data, error message: "+
		"CSV table encountered too many errors, giving up. Line %d: %s. Nothing was loaded.", c.line, what)}
}

// dialectRecords writes the records of data, read by d, without its first
// skip, to cw as the emulator reads CSV. Every record is made as wide as
// *width, which the first record sets when it is 0: with allowJaggedRows,
// a short one is filled with nulls. A value that is d's null marker is
// written as a null, an empty value; with a custom null marker, an empty
// value that was in the data fails the load (see above).
func dialectRecords(cw *csv.Writer, data io.Reader, d csvDialect, cols []field, skip int64, width *int) error {
	rd := &csvRecords{r: bufio.NewReader(data), d: d}
	for n := int64(0); ; n++ {
		rec, quoted, err := rd.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n < skip {
			continue
		}
		if *width == 0 {
			*width = len(rec)
		}
		given := len(rec)
		if d.jagged {
			for len(rec) < *width {
				rec = append(rec, "")
			}
		}
		for i := 0; d.nullIsSet && i < given; i++ {
			switch {
			case rec[i] == d.nullMark && !quoted[i]:
				rec[i] = ""
			case rec[i] == "":
				return emptyValue(rd.line, i, cols)
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
}

// emptyValue is the failure for an empty value in field i of a record
// ending at line, under a custom nullMarker.
func emptyValue(line, i int, cols []field) error {
	name, typ := fmt.Sprintf("column %d", i+1), ""
	if i < len(cols) {
		name, typ = cols[i].Name, strings.ToUpper(cols[i].Type)
	}
	switch typ {
	case "STRING", "BYTES", "":
		return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: an empty value "+
			"(line %d, %s) in a CSV load with a nullMarker. BigQuery loads it into a STRING or BYTES column as an empty "+
			"value, but the emulator behind CloudBurrow loads every empty CSV value as NULL (measured). Nothing was loaded.",
			line, name)}
	}
	return &loadDataError{code: 400, reason: "invalid", msg: fmt.Sprintf("Error while reading data, error message: "+
		"CSV table encountered too many errors, giving up. Line %d: %s (%s) is empty, and with a nullMarker set an empty "+
		"value is not NULL. Nothing was loaded.", line, name, typ)}
}
