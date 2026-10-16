package bigqueryfront

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
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
// an empty value in a column that is not STRING or BYTES, which BigQuery
// refuses (400 invalid; in a STRING or BYTES column, where BigQuery loads
// an empty string, it is sent as emptyStringMark, which the emulator's
// patch 0038 loads as one, #1457); and a quote that is not closed,
// or data between a closing quote and the next delimiter (400 invalid). A
// fieldDelimiter or quote of more than one byte, or outside ASCII, is 501
// before anything is read.

// The other CSV options of a load (#952), quoted from the same reference:
//
//   - allowQuotedNewlines: "Indicates if BigQuery should allow quoted data
//     sections that contain newline characters in a CSV file. The default
//     value is false."
//   - encoding: "The supported values are UTF-8, ISO-8859-1, UTF-16BE,
//     UTF-16LE, UTF-32BE, and UTF-32LE. The default value is UTF-8.
//     BigQuery decodes the data after the raw, binary data has been split
//     using the values of the quote and fieldDelimiter properties."
//   - maxBadRecords: "The maximum number of bad records that BigQuery can
//     ignore when running the job. If the number of bad records exceeds
//     this value, an invalid error is returned in the job result. The
//     default value is 0, which requires that all records are valid."
//   - ignoreUnknownValues: "If true, the extra values are ignored. If
//     false, records with extra columns are treated as bad records ...
//     CSV: Trailing columns". allowJaggedRows: "If false, records with
//     missing trailing columns are treated as bad records".
//   - nullMarkers: "A list of strings represented as SQL NULL value in a
//     CSV file. null_marker and null_markers can't be set at the same
//     time. ... If both null_marker and null_markers are set at the same
//     time, a user error would be thrown. Any strings listed in
//     null_markers, including empty string would be interpreted as SQL
//     NULL. This applies to all column types."
//   - preserveAsciiControlCharacters: "whether the embedded ASCII control
//     characters (the first 32 characters in the ASCII-table, from '\x00'
//     to '\x1F') are preserved."
//   - sourceColumnMatch: "POSITION - Matches by position. This assumes
//     that the columns are ordered the same way as the schema. NAME -
//     Matches by name. This reads the header row as column names and
//     reorders columns to match the field names in the schema."
//   - timeZone, dateFormat, datetimeFormat, timeFormat, timestampFormat:
//     the time zone and formats DATE, DATETIME, TIME and TIMESTAMP values
//     are parsed with.
//
// The emulator uses none of them (measured against the pinned image
// through the official Go client, with a schema and a header row, which
// it always takes as one): it loaded a quoted newline whatever
// allowQuotedNewlines said; with encoding ISO-8859-1 it stored the byte
// 0xE9 as U+FFFD, not é; with maxBadRecords 1 it failed a load with one
// bad value (400 "strconv.ParseInt") or one row too wide (400 "wrong
// number of fields"), and with ignoreUnknownValues it failed the wide row
// the same way; it loaded the values of nullMarkers as text; it stored
// the control characters \x00 and \x01 as they were whatever
// preserveAsciiControlCharacters said; it stored a TIMESTAMP with no
// zone as UTC whatever timeZone said, and failed a DATE in dateFormat
// MM/DD/YYYY (400 "failed to convert"). It also stored NULL in a REQUIRED
// column given an empty value (and failed a short row with
// allowJaggedRows, #945). It does match a header's names to the columns
// by name: a file whose header named them in another order loaded each
// value into its column.
//
// So the front reads each record by these rules too (dialectRecords), as
// the data passes:
//
//   - A newline inside a quoted value fails the load, 400 invalid, unless
//     allowQuotedNewlines is true; with maxBadRecords set it is 501, as
//     BigQuery's reading of the rest of such a file is not documented.
//   - ISO-8859-1 data is decoded to the UTF-8 the emulator reads, and so
//     are UTF-16BE, UTF-16LE, UTF-32BE and UTF-32LE (#1325, csvtimes.go).
//   - A record is bad when it is too short (without allowJaggedRows), too
//     wide (without ignoreUnknownValues, which drops the trailing values
//     instead), or when a REQUIRED column would get NULL. Up to
//     maxBadRecords bad records are left out, and one more fails the load,
//     400 invalid. A value the emulator then fails on is 501 when
//     maxBadRecords is set: BigQuery may have counted it as a bad record.
//     The job reports the records left out (loadstats.go, #960).
//   - Each value of nullMarkers is written as NULL; an empty value is
//     NULL only when "" is one of them, and is otherwise 501 (the
//     reference does not say what BigQuery does with it). Both nullMarker
//     and nullMarkers is 400 invalid.
//   - Without preserveAsciiControlCharacters, a value holding a control
//     character other than tab, newline and carriage return is 501: the
//     reference does not say what BigQuery does with it. With it, the
//     value is loaded as it is, as the emulator stores it.
//   - With sourceColumnMatch NAME, "Column names are read from the last
//     skipped row based on Header rows to skip" (loading-data-cloud-
//     storage-csv): row skipLeadingRows of each file is read as the header
//     (#1457; before it, only skipLeadingRows 1), and each value goes to
//     the column it names (case-insensitively); a header that does not
//     name each column once is 501, as is NAME with no row skipped.
//     POSITION is the default for a load with columns.
//   - timeZone and the four formats are read as BigQuery reads them, and
//     each value written in the form the emulator reads (#1325,
//     csvtimes.go).
//
// An empty quoted value ("") in a REQUIRED STRING or BYTES column is 501:
// the emulator loads it as NULL, and BigQuery may load it as an empty
// string.

// csvDialect is how a CSV load's data is read.
type csvDialect struct {
	delim byte
	// quote is 0 for none.
	quote     byte
	jagged    bool
	nullMark  string
	nullIsSet bool
	// markers are nullMarkers, set when markersSet (#952).
	markers    map[string]bool
	markersSet bool
	// quotedNewlines is allowQuotedNewlines.
	quotedNewlines bool
	// latin1 is encoding ISO-8859-1.
	latin1 bool
	// delimText is the fieldDelimiter as the load gave it, when it is not
	// the default or \t: delim is its first byte, which withOptions
	// changes for ISO-8859-1 data (#1457, dialectOf).
	delimText string
	// repair is UTF-8 data split on a byte outside ASCII: a value's bytes
	// that are not UTF-8 are each read as U+FFFD (#1457, dialectOf).
	repair bool
	// wide is encoding UTF-16BE, UTF-16LE, UTF-32BE or UTF-32LE (#1325).
	wide string
	// times are timeZone and the date and time formats (#1325,
	// csvtimes.go), or nil.
	times *csvTimes
	// ignoreUnknown is ignoreUnknownValues.
	ignoreUnknown bool
	// maxBad is maxBadRecords.
	maxBad int64
	// keepControl is preserveAsciiControlCharacters.
	keepControl bool
	// byName is sourceColumnMatch NAME, with columns given.
	byName bool
	// required is whether a column the load's values go to is REQUIRED.
	required bool
	// values is whether a column the load's values go to is BYTES or
	// FLOAT64, whose values the front reads (#1065, #1066,
	// storedvalues.go).
	values bool
}

// plain reports whether the emulator's own reading of the data is
// BigQuery's under d, so the front need not read the records: the default
// delimiter and quote, allowQuotedNewlines and
// preserveAsciiControlCharacters set, no other option but a
// sourceColumnMatch of POSITION (#952), no REQUIRED column, and no BYTES
// one (#1065, #1075: its values are checked as base64).
func (d csvDialect) plain() bool {
	return d.delim == ',' && d.quote == '"' && !d.jagged && !d.nullIsSet && !d.markersSet && d.quotedNewlines &&
		!d.latin1 && d.wide == "" && d.times == nil && !d.ignoreUnknown && d.maxBad == 0 && d.keepControl && !d.byName &&
		!d.required && !d.values
}

// optionsSet reports whether d has any option the load set, beyond the
// defaults of allowQuotedNewlines and preserveAsciiControlCharacters and
// the check of REQUIRED columns, which a load the front does not read is
// left without.
func (d csvDialect) optionsSet() bool {
	return !(d.delim == ',' && d.quote == '"' && !d.jagged && !d.nullIsSet && !d.markersSet && !d.latin1 &&
		d.wide == "" && d.times == nil && !d.ignoreUnknown && d.maxBad == 0 && !d.byName)
}

// dialectOf returns the dialect a load's options give, or why it is 501.
//
// fieldDelimiter "is interpreted as a single byte. For files encoded in
// ISO-8859-1, any single character can be used as a separator. For files
// encoded in UTF-8, characters represented in decimal range 1-127
// (U+0001-U+007F) can be used without any modification. UTF-8 characters
// encoded with multiple bytes (i.e. U+0080 and above) will have only the
// first byte used for separating fields. The remaining bytes will be
// treated as a part of the field"; quote: "BigQuery converts the string to
// ISO-8859-1 encoding, and then uses the first byte of the encoded string
// to split the data in its raw, binary state"; and "BigQuery decodes the
// data after the raw, binary data has been split using the values of the
// quote and fieldDelimiter properties" (JobConfigurationLoad). So the
// delimiter is one byte: in ISO-8859-1 data the ISO-8859-1 byte of its
// first character (#1457), and in UTF-8 data the first byte of its UTF-8
// encoding (#1457, second part), which is also how a delimiter of several
// characters is read; the quote is the ISO-8859-1 byte of its first
// character. The CSV loading page says instead that an ISO-8859-1
// character outside ASCII "will interpret it correctly" as a delimiter in
// UTF-8 data; the front follows the API reference, which also matches
// that page's "if you use a multibyte character as a delimiter, some of
// the bytes will be interpreted incorrectly as part of the field value".
// In UTF-8 data split on a byte outside ASCII, a value left with bytes
// that are not UTF-8 has each such byte read as U+FFFD: "If BigQuery can't
// convert a character other than the ASCII 0 character, BigQuery converts
// the character to the standard Unicode replacement character" (the
// encoding field). Still 501: a quote, or in ISO-8859-1 data a delimiter,
// whose first character is outside ISO-8859-1, and a delimiter of several
// bytes or outside ASCII in UTF-16 or UTF-32 data (withOptions), none of
// which the reference describes.
func dialectOf(fieldDelimiter string, quote *string, jagged bool, nullMarker *string) (csvDialect, string) {
	d := csvDialect{delim: ',', quote: '"', jagged: jagged}
	switch fd := fieldDelimiter; {
	case fd == "":
	case fd == `\t`:
		d.delim = '\t'
	default:
		d.delimText = fd
		if b := fd[0]; b != 0 && b != '\n' && b != '\r' {
			d.delim = b
		} else {
			return d, fmt.Sprintf("fieldDelimiter %q, whose first byte is a NUL or a line break, which BigQuery's "+
				"reference does not document as a separator", fd)
		}
	}
	if quote != nil {
		switch q := *quote; {
		case q == "":
			d.quote = 0
		default:
			b, ok := latin1Byte(q)
			if !ok {
				return d, fmt.Sprintf("quote %q: BigQuery converts the quote to ISO-8859-1 and uses its first byte, "+
					"and its first character has none CloudBurrow reads", q)
			}
			d.quote = b
		}
	}
	if d.quote != 0 && d.quote == d.delim {
		return d, fmt.Sprintf("quote %q is also the fieldDelimiter", string(rune(d.quote)))
	}
	if nullMarker != nil && *nullMarker != "" {
		d.nullMark, d.nullIsSet = *nullMarker, true
	}
	return d, ""
}

// latin1Byte returns the ISO-8859-1 byte of the first character of s,
// false when it has none (or is NUL or a line break).
func latin1Byte(s string) (byte, bool) {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && n <= 1 || r < 1 || r > 0xFF || r == '\n' || r == '\r' {
		return 0, false
	}
	return byte(r), true
}

// csvOptions are the CSV options of a load besides those dialectOf reads
// (#952).
type csvOptions struct {
	AllowQuotedNewlines            bool            `json:"allowQuotedNewlines"`
	Encoding                       string          `json:"encoding"`
	MaxBadRecords                  json.RawMessage `json:"maxBadRecords"`
	IgnoreUnknownValues            bool            `json:"ignoreUnknownValues"`
	PreserveAsciiControlCharacters bool            `json:"preserveAsciiControlCharacters"`
	NullMarkers                    []string        `json:"nullMarkers"`
	SourceColumnMatch              string          `json:"sourceColumnMatch"`
	TimeZone                       string          `json:"timeZone"`
	DateFormat                     string          `json:"dateFormat"`
	DatetimeFormat                 string          `json:"datetimeFormat"`
	TimeFormat                     string          `json:"timeFormat"`
	TimestampFormat                string          `json:"timestampFormat"`
}

// withOptions returns d with o's options, for a load whose values go to
// cols (none when they are detected) with skipLeadingRows skip; or, when
// the load is refused before its data is read, the status, reason and
// message (see above).
func (d csvDialect) withOptions(o csvOptions, nullMarkerSet bool, cols []field, skip int64) (csvDialect, int, string, string) {
	unimplemented := func(why string) (csvDialect, int, string, string) {
		return d, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load with " + why + ". Nothing was loaded."
	}
	times, code, why := csvTimesOf(o, len(cols) == 0) // #1325, csvtimes.go
	switch code {
	case http.StatusNotImplemented:
		return unimplemented(why)
	case http.StatusBadRequest:
		return d, code, "invalid", why
	}
	d.times = times
	d.quotedNewlines = o.AllowQuotedNewlines
	d.ignoreUnknown = o.IgnoreUnknownValues
	d.keepControl = o.PreserveAsciiControlCharacters
	switch strings.ToUpper(o.Encoding) {
	case "", "UTF-8", "UTF8":
	case "ISO-8859-1":
		d.latin1 = true
	}
	switch strings.ToUpper(o.Encoding) {
	case "", "UTF-8", "UTF8", "ISO-8859-1":
	case "UTF-16BE", "UTF-16LE", "UTF-32BE", "UTF-32LE":
		d.wide = strings.ToUpper(o.Encoding) // #1325, csvtimes.go
	default:
		return d, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid encoding %q: the supported values are UTF-8, "+
			"ISO-8859-1, UTF-16BE, UTF-16LE, UTF-32BE and UTF-32LE.", o.Encoding)
	}
	switch {
	case d.latin1 && d.delimText != "":
		b, ok := latin1Byte(d.delimText)
		if !ok {
			return unimplemented(fmt.Sprintf("fieldDelimiter %q in ISO-8859-1 data: BigQuery reads \"any single "+
				"character\" of ISO-8859-1 there, and does not document one outside it", d.delimText))
		}
		d.delim = b
	case d.wide != "" && (len(d.delimText) > 1 || d.delim >= 0x80 || d.quote >= 0x80):
		return unimplemented(fmt.Sprintf("a fieldDelimiter or quote of several bytes or outside ASCII in %s data, "+
			"which BigQuery's reference, whose separator \"is interpreted as a single byte\", does not describe", d.wide))
	case !d.latin1 && d.wide == "" && (d.delim >= 0x80 || d.quote >= 0x80):
		d.repair = true
	}
	if d.quote != 0 && d.quote == d.delim {
		return d, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid quote %q: in this data it is the same byte as "+
			"the fieldDelimiter %q.", string(rune(d.quote)), d.delimText)
	}
	if n, set := skipLeadingRows(o.MaxBadRecords); set {
		if n < 0 {
			return d, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid maxBadRecords %d: it cannot be negative.", n)
		}
		d.maxBad = n
	}
	if o.NullMarkers != nil {
		if nullMarkerSet {
			return d, http.StatusBadRequest, "invalid", "nullMarker and nullMarkers cannot both be set."
		}
		d.markers, d.markersSet = map[string]bool{}, true
		for _, m := range o.NullMarkers {
			d.markers[m] = true
		}
	}
	switch strings.ToUpper(o.SourceColumnMatch) {
	case "", "SOURCE_COLUMN_MATCH_UNSPECIFIED", "POSITION":
	case "NAME":
		if len(cols) > 0 {
			if skip < 1 {
				return unimplemented(fmt.Sprintf("sourceColumnMatch NAME and skipLeadingRows %d. BigQuery reads the "+
					"column names \"from the last skipped row\" (loading-data-cloud-storage-csv), and with none skipped "+
					"its documentation does not say where they are", skip))
			}
			d.byName = true
		}
	default:
		return unimplemented(fmt.Sprintf("sourceColumnMatch %q, which CloudBurrow does not read", o.SourceColumnMatch))
	}
	for _, c := range cols {
		if strings.EqualFold(c.Mode, "REQUIRED") {
			d.required = true
		}
	}
	d.values = storedValues(cols)
	return d, 0, "", ""
}

// loadDataError is why the front failed a load while its data passed
// through it.
type loadDataError struct {
	code   int
	reason string
	msg    string
}

func (e *loadDataError) Error() string { return e.msg }

// dataFailure holds the loadDataError a load's data stream ended with,
// or, when it ended without one, what it counted (#960, loadstats.go).
type dataFailure struct {
	mu      sync.Mutex
	err     *loadDataError
	counted *loadCounts
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
	// skipping is whether the record read next is one skipLeadingRows
	// skips.
	skipping bool
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
		v := f.String()
		if c.d.latin1 {
			v = latin1String(v) // split first, then decoded (#1457)
		} else if c.d.repair {
			v = repairUTF8(v)
		}
		rec = append(rec, v)
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
				if !c.d.quotedNewlines {
					if c.skipping {
						return nil, nil, false, &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf(
							"Not implemented here: a newline inside a quoted value (line %d), in a row skipLeadingRows "+
								"skips, of a CSV load without allowQuotedNewlines. BigQuery then reads the file as lines, "+
								"which skipLeadingRows counts, but CloudBurrow cannot tell which lines those are. Nothing "+
								"was loaded. Set allowQuotedNewlines if the value is meant to hold the newline.", c.line)}
					}
					if c.d.maxBad > 0 {
						return nil, nil, false, &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf(
							"Not implemented here: a newline inside a quoted value (line %d) of a CSV load without "+
								"allowQuotedNewlines and with maxBadRecords. BigQuery refuses the record, but how it reads "+
								"the rest of the file then is not documented. Nothing was loaded.", c.line)}
					}
					return nil, nil, false, c.invalid("a quoted value holds a newline, and allowQuotedNewlines is false")
				}
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

// csvState is what reading a load's data keeps from one file to the
// next: the width of a record, the bad records left out, and what the
// job reports of the load (#960): the records written on, and the first
// maxListedBadRecords bad records, each an error located at loc, the file
// being read (its gs:// URI, or "" for an upload).
type csvState struct {
	width int
	bad   int64
	rows  int64
	loc   string
	errs  []rowError
}

// dialectRecords writes the records of data, read by d, without its first
// skip, to cw as the emulator reads CSV. Every record is made as wide as
// st.width, which the first record sets when it is 0: with
// allowJaggedRows, a short one is filled with nulls, and with
// ignoreUnknownValues a wide one loses its trailing values. A value that
// is a null marker is written as a null, an empty value; with a custom
// null marker, an empty value that was in the data fails the load (see
// above). A bad record is left out, up to d.maxBad of them over the load.
func dialectRecords(cw *csv.Writer, data io.Reader, d csvDialect, cols []field, skip int64, st *csvState) error {
	if d.wide != "" {
		data = wideReader(data, d.wide) // #1325, csvtimes.go
	}
	rd := &csvRecords{r: bufio.NewReader(data), d: d}
	var order []int // with byName, the column each position of a record goes to
	for n := int64(0); ; n++ {
		rd.skipping = n < skip
		rec, quoted, err := rd.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.byName && n == skip-1 {
			if order, err = headerOrder(rec, cols, rd.line); err != nil {
				return err
			}
		}
		if n < skip {
			continue
		}
		if !d.keepControl {
			if err := controlCharacters(rec, rd.line); err != nil {
				return err
			}
		}
		if st.width == 0 {
			st.width = len(rec)
		}
		width := st.width
		if order != nil {
			width = len(order)
		}
		var bad string
		given := len(rec)
		switch {
		case len(rec) > width && d.ignoreUnknown:
			rec, quoted, given = rec[:width], quoted[:width], width
		case len(rec) > width:
			bad = fmt.Sprintf("it has %d values, more than the %d columns, and ignoreUnknownValues is false", len(rec), width)
		case len(rec) < width && !d.jagged:
			bad = fmt.Sprintf("it has %d values, fewer than the %d columns, and allowJaggedRows is false", len(rec), width)
		}
		isGiven := make([]bool, width)
		for i := range isGiven {
			isGiven[i] = i < given
		}
		for len(rec) < width {
			rec, quoted = append(rec, ""), append(quoted, false)
		}
		if bad == "" && order != nil {
			out, q, g := make([]string, len(order)), make([]bool, len(order)), make([]bool, len(order))
			for i, col := range order {
				out[col], q[col], g[col] = rec[i], quoted[i], isGiven[i]
			}
			rec, quoted, isGiven = out, q, g
		}
		for i := 0; bad == "" && i < len(rec); i++ {
			if !isGiven[i] {
				continue
			}
			switch {
			case d.nullIsSet && rec[i] == d.nullMark && !quoted[i], d.markersSet && d.markers[rec[i]] && !quoted[i]:
				rec[i] = ""
			case d.markersSet && rec[i] == "" && d.markers[""]:
			case d.markersSet && rec[i] == "":
				return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: an "+
					"empty value (line %d, %s) in a CSV load with nullMarkers that do not include the empty string. "+
					"BigQuery reads the values of nullMarkers as NULL, but does not document what it does with an empty "+
					"value then. Nothing was loaded.", rd.line, columnName(i, cols))}
			case d.nullIsSet && rec[i] == "":
				if err := emptyValue(rd.line, i, cols); err != nil {
					return err
				}
				rec[i] = emptyStringMark
			case rec[i] == emptyStringMark:
				return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: "+
					"the value (line %d, %s) is the text CloudBurrow sends its emulator for an empty string. Nothing was "+
					"loaded.", rd.line, columnName(i, cols))}
			}
		}
		for i := 0; bad == "" && i < len(rec) && i < len(cols); i++ {
			if rec[i] != "" || !strings.EqualFold(cols[i].Mode, "REQUIRED") {
				continue
			}
			typ := strings.ToUpper(cols[i].Type)
			if i < len(quoted) && quoted[i] && !d.nullIsSet && !d.markersSet && (typ == "STRING" || typ == "BYTES") {
				return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: an "+
					"empty quoted value (line %d) in the REQUIRED %s column %s. The emulator behind CloudBurrow loads it as "+
					"NULL (measured), and BigQuery may load it as an empty string. Nothing was loaded.", rd.line, typ,
					cols[i].Name)}
			}
			bad = fmt.Sprintf("the REQUIRED column %s has no value", cols[i].Name)
		}
		if bad == "" && d.times != nil {
			bad = d.times.convert(rec, cols) // #1325, csvtimes.go
		}
		if bad != "" {
			st.bad++
			if st.bad > d.maxBad {
				return rd.invalid(fmt.Sprintf("the record is bad: %s (%d bad records, and maxBadRecords is %d)", bad,
					st.bad, d.maxBad))
			}
			if len(st.errs) < maxListedBadRecords {
				st.errs = append(st.errs, rowError{Reason: "invalid", Location: st.loc, Message: fmt.Sprintf(
					"Error while reading data, error message: Line %d: the record is bad: %s. It was left out "+
						"(%d of maxBadRecords %d).", rd.line, bad, st.bad, d.maxBad)})
			}
			continue
		}
		if d.values {
			if err := csvValues(rec, cols, rd.line); err != nil {
				return err
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
		st.rows++
	}
}

// headerOrder returns, for sourceColumnMatch NAME, the column of cols
// each value of a record goes to, which header, the header row ending at
// line, names.
func headerOrder(header []string, cols []field, line int) ([]int, error) {
	index := map[string]int{}
	for i, c := range cols {
		index[strings.ToLower(c.Name)] = i
	}
	order := make([]int, len(header))
	seen := map[int]bool{}
	for i, h := range header {
		col, ok := index[strings.ToLower(strings.TrimSpace(h))]
		if !ok || seen[col] {
			break
		}
		order[i], seen[col] = col, true
	}
	if len(seen) != len(cols) || len(header) != len(cols) {
		return nil, &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: a CSV "+
			"load with sourceColumnMatch NAME whose header (line %d) does not name each column of the schema once. "+
			"CloudBurrow matches a header that does, and BigQuery's handling of another is not documented. Nothing was "+
			"loaded.", line)}
	}
	return order, nil
}

// controlCharacters fails a record holding a control character other
// than tab, newline and carriage return, without
// preserveAsciiControlCharacters (see above).
func controlCharacters(rec []string, line int) error {
	for _, v := range rec {
		for i := 0; i < len(v); i++ {
			if c := v[i]; c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
				return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: the "+
					"control character \\x%02X (line %d) in a CSV load without preserveAsciiControlCharacters. BigQuery "+
					"preserves it only with that option and does not document what it does otherwise; the emulator behind "+
					"CloudBurrow stores it as it is (measured). Nothing was loaded. Set preserveAsciiControlCharacters to "+
					"load it.", c, line)}
			}
		}
	}
	return nil
}

// latin1String decodes ISO-8859-1 to UTF-8: each byte is the code point
// of its value.
func latin1String(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			b := make([]rune, len(s))
			for k := 0; k < len(s); k++ {
				b[k] = rune(s[k])
			}
			return string(b)
		}
	}
	return s
}

// repairUTF8 returns s with each byte that is not part of a UTF-8
// character replaced by U+FFFD (see dialectOf).
func repairUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		b.WriteRune(r) // RuneError for a byte that is not UTF-8
		s = s[n:]
	}
	return b.String()
}

// columnName names column i of cols, or its position.
func columnName(i int, cols []field) string {
	if i < len(cols) {
		return cols[i].Name
	}
	return fmt.Sprintf("column %d", i+1)
}

// emptyStringMark is how the front writes an empty string to the
// emulator, which loads an empty CSV value as NULL: its patch 0038
// (#1457) loads this value as an empty string.
const emptyStringMark = "\uFDD0cloudburrow:empty-string\uFDD0"

// emptyValue is the failure for an empty value in field i of a record
// ending at line, under a custom nullMarker, or nil for a STRING or BYTES
// column, where "BigQuery interprets the empty string as an empty value"
// (nullMarker, JobConfigurationLoad): the front sends it as
// emptyStringMark (#1457).
func emptyValue(line, i int, cols []field) error {
	name, typ := columnName(i, cols), ""
	if i < len(cols) {
		typ = strings.ToUpper(cols[i].Type)
	}
	switch typ {
	case "STRING", "BYTES":
		return nil
	case "":
		return &loadDataError{code: 501, reason: "notImplemented", msg: fmt.Sprintf("Not implemented here: an empty value "+
			"(line %d, %s) beyond the table's columns in a CSV load with a nullMarker. Nothing was loaded.", line, name)}
	}
	return &loadDataError{code: 400, reason: "invalid", msg: fmt.Sprintf("Error while reading data, error message: "+
		"CSV table encountered too many errors, giving up. Line %d: %s (%s) is empty, and with a nullMarker set an empty "+
		"value is not NULL. Nothing was loaded.", line, name, typ)}
}

// csvValues changes each BYTES value of rec, a record ending at line of a
// CSV load into cols, to its standard base64, which the emulator decodes,
// and fails the load on one that is not base64 (#1065, #1075,
// storedvalues.go). An empty value is NULL, and left.
func csvValues(rec []string, cols []field, line int) error {
	for i := 0; i < len(rec) && i < len(cols); i++ {
		if rec[i] == "" || rec[i] == emptyStringMark {
			continue
		}
		loc := fmt.Sprintf("line %d, column %s", line, cols[i].Name)
		switch strings.ToUpper(cols[i].Type) {
		case "BYTES":
			s, p := decodeBytes(rec[i], "a CSV load's record", loc, true)
			if p != nil {
				return p.loadError()
			}
			rec[i] = s
		}
	}
	return nil
}
