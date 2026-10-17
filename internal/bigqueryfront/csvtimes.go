package bigqueryfront

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront/castformat"
	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront/strftime"
)

// A CSV load's timeZone, dateFormat, datetimeFormat, timeFormat and
// timestampFormat, and its encodings UTF-16BE, UTF-16LE, UTF-32BE and
// UTF-32LE (#1325). The REST reference:
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad
//
//   - timeZone: "Default time zone that will apply when parsing timestamp
//     values that have no specific time zone." A zone is a name of the tz
//     database or an offset, (+|-)H[H][:M[M]] (data-types#time_zone_name);
//     another is 400 invalid.
//   - dateFormat, datetimeFormat, timeFormat, timestampFormat: "Date
//     format used for parsing DATE [DATETIME, TIME, TIMESTAMP] values." The
//     console's help for them (loading-data-cloud-storage-csv) says they
//     are format elements, "SQL styles format (for example, MM/DD/YYYY)",
//     and "If this value is present, this format is the only compatible
//     DATE format": the elements of format_string_as_datetime
//     (format-elements), which the front reads with the code the engine's
//     CAST ... FORMAT does (castformat, a copy of the engine patch's). A
//     format those elements cannot parse with is 400 invalid. The official
//     Go client's FileConfig, and the REST discovery document it is made
//     from, say each "Supports C-style and SQL-style values"; the REST
//     reference does not say which C-style elements. A C-style format
//     (one holding %, #1338) is read with GoogleSQL's format elements for
//     PARSE_DATE, PARSE_DATETIME, PARSE_TIME and PARSE_TIMESTAMP (%Y, %m,
//     %d, %H, %M, %S, %Ez, ...: format-elements#format_elements_date_time),
//     by the engine's own parser (strftime, a copy of googlesqlite's, with
//     each literal character held to the value's). An element the type
//     does not take (%H in a dateFormat) or that is not one is 400
//     invalid. A TIMESTAMP whose format reads no zone (%z, %Z, %Ez or %s)
//     is in timeZone, or UTC.
//   - encoding: "The supported values are UTF-8, ISO-8859-1, UTF-16BE,
//     UTF-16LE, UTF-32BE, and UTF-32LE." ... "If BigQuery can't convert a
//     character ..., BigQuery converts the character to the standard
//     Unicode replacement character". Another value is 400 invalid.
//
// The emulator uses none of them (measured against the pinned image
// through the official Go client: a TIMESTAMP with no zone was stored as
// UTC whatever timeZone said, a DATE in dateFormat MM/DD/YYYY failed the
// load, 400 "failed to convert", and it reads every load as UTF-8). So the
// front reads the records itself (dialectRecords) and writes each value
// of a DATE, DATETIME, TIME or TIMESTAMP column with a format set, and each
// TIMESTAMP value with no zone under timeZone, in the one form the
// emulator reads; a value the format does not parse makes its record bad,
// left out under maxBadRecords and otherwise failing the load, 400
// invalid. With timeZone and no timestampFormat, a value is read by the
// default forms (YYYY-MM-DD or YYYY/MM/DD, then T or a space and
// HH:MM[:SS[.SSSSSS]]): one with no zone after its time is in timeZone,
// and any other is sent as it was. A UTF-16 or UTF-32 file is decoded to
// UTF-8 before it is read, a byte-order mark at its start dropped, and an
// unpaired surrogate or a value outside Unicode read as U+FFFD: the
// reference's "BigQuery decodes the data after the raw, binary data has
// been split using the values of the quote and fieldDelimiter properties"
// cannot be meant byte for byte for these encodings, whose comma is two or
// four bytes.
//
// With autodetect and no columns given, BigQuery's detection "will also
// decide DATE column type based on this format", and the emulator detects
// the columns by its own rules: the front detects them instead (#1338,
// csvdetect.go), so a load reaching csvTimesOf with detected columns is
// one it did not detect (into another project's table), which is 501.

// csvTimes are a CSV load's timeZone and formats.
type csvTimes struct {
	loc     *time.Location // timeZone, or UTC
	zoneSet bool
	formats map[string]string // by column type: DATE, DATETIME, TIME, TIMESTAMP
	// now is the instance's clock (#1487), which a format without a
	// year reads the year from; nil is the system's.
	now func() time.Time
}

// csvTimesOf reads o's timeZone and formats, for a load whose columns are
// detected when detected. It returns nil when none is set, or the status
// and why the load is refused.
func csvTimesOf(o csvOptions, detected bool) (*csvTimes, int, string) {
	t := &csvTimes{loc: time.UTC, formats: map[string]string{}}
	set := []string{}
	for _, f := range []struct {
		name, value, typ string
		kind             castformat.Kind
		cKind            strftime.Kind
	}{
		{"dateFormat", o.DateFormat, "DATE", castformat.Date, strftime.Date},
		{"datetimeFormat", o.DatetimeFormat, "DATETIME", castformat.Datetime, strftime.Datetime},
		{"timeFormat", o.TimeFormat, "TIME", castformat.Time, strftime.Time},
		{"timestampFormat", o.TimestampFormat, "TIMESTAMP", castformat.Timestamp, strftime.Timestamp},
	} {
		if f.value == "" {
			continue
		}
		set = append(set, f.name)
		if strftime.IsCStyle(f.value) { // #1338, strftime
			if err := strftime.Validate(f.value, f.cKind); err != nil {
				return nil, http.StatusBadRequest, fmt.Sprintf("Invalid %s %q: %v.", f.name, f.value, err)
			}
		} else if err := castformat.ValidateParse(f.value, f.kind); err != nil {
			return nil, http.StatusBadRequest, fmt.Sprintf("Invalid %s %q: %v.", f.name, f.value, err)
		}
		t.formats[f.typ] = f.value
	}
	if o.TimeZone != "" {
		set = append(set, "timeZone")
		loc, err := loadZone(o.TimeZone)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Sprintf("Invalid timeZone %q: it is a time zone name of the tz "+
				"database, such as America/Los_Angeles, or an offset from UTC, such as -08:00.", o.TimeZone)
		}
		t.loc, t.zoneSet = loc, true
	}
	if len(set) == 0 {
		return nil, 0, ""
	}
	if detected {
		return nil, http.StatusNotImplemented, strings.Join(set, ", ") + " and autodetect with no schema. BigQuery " +
			"detects the columns' types by the formats, but the emulator behind CloudBurrow detects them by its own " +
			"rules. Give the load a schema"
	}
	return t, 0, ""
}

var zoneOffset = regexp.MustCompile(`^(?:UTC)?([+-])([0-9]{1,2})(?::([0-9]{1,2}))?$`)

// loadZone reads a GoogleSQL time zone: a tz database name, or an offset.
func loadZone(s string) (*time.Location, error) {
	if m := zoneOffset.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[2])
		mins := 0
		if m[3] != "" {
			mins, _ = strconv.Atoi(m[3])
		}
		if h > 14 || mins > 59 {
			return nil, errors.New("offset out of range")
		}
		off := h*3600 + mins*60
		if m[1] == "-" {
			off = -off
		}
		return time.FixedZone(s, off), nil
	}
	if s == "" || strings.EqualFold(s, "local") {
		return nil, errors.New("not a zone")
	}
	return time.LoadLocation(s)
}

// defaultTimestamp is a TIMESTAMP in a default form: the date, the time,
// and whatever follows the time (its zone, if any).
var defaultTimestamp = regexp.MustCompile(`^([0-9]{4})[-/]([0-9]{1,2})[-/]([0-9]{1,2})` +
	`(?:[ T]([0-9]{1,2}):([0-9]{2})(?::([0-9]{2})(?:\.([0-9]{1,6}))?)?)?(.*)$`)

// The forms the emulator reads a CSV's values in.
const (
	emulatorDate      = "2006-01-02"
	emulatorDatetime  = "2006-01-02 15:04:05.999999"
	emulatorTime      = "15:04:05.999999"
	emulatorTimestamp = "2006-01-02 15:04:05.999999-07:00"
)

// convert rewrites the values of rec, a record of a load into cols, by t
// (above). It returns why the record is bad, or "".
func (t *csvTimes) convert(rec []string, cols []field) string {
	clock := time.Now
	if t.now != nil {
		clock = t.now
	}
	now := clock().In(t.loc)
	for i := 0; i < len(rec) && i < len(cols); i++ {
		v := rec[i]
		if v == "" {
			continue // NULL
		}
		typ := legacyType(strings.ToUpper(cols[i].Type))
		switch typ {
		case "FLOAT", "INTEGER", "BOOLEAN", "RECORD":
			continue
		}
		format, ok := t.formats[typ]
		if !ok {
			if typ == "TIMESTAMP" && t.zoneSet {
				if out, ok := zonedTimestamp(v, t.loc); ok {
					rec[i] = out
				}
			}
			continue
		}
		var kind castformat.Kind
		var layout string
		switch typ {
		case "DATE":
			kind, layout = castformat.Date, emulatorDate
		case "DATETIME":
			kind, layout = castformat.Datetime, emulatorDatetime
		case "TIME":
			kind, layout = castformat.Time, emulatorTime
		case "TIMESTAMP":
			kind, layout = castformat.Timestamp, emulatorTimestamp
		}
		var got time.Time
		var err error
		if strftime.IsCStyle(format) { // #1338
			got, err = strftime.Parse(format, strings.TrimSpace(v), cKinds[kind], t.loc)
		} else {
			got, err = castformat.ParseDatetime(strings.TrimSpace(v), kind, format, t.loc, now)
		}
		if err != nil {
			return fmt.Sprintf("the value %q of the %s column %s does not match its format %q (%v)", v, typ,
				cols[i].Name, format, err)
		}
		if kind == castformat.Timestamp {
			got = got.UTC()
		}
		rec[i] = got.Format(layout)
	}
	return ""
}

// cKinds are the strftime kinds of castformat's.
var cKinds = map[castformat.Kind]strftime.Kind{castformat.Date: strftime.Date, castformat.Datetime: strftime.Datetime,
	castformat.Time: strftime.Time, castformat.Timestamp: strftime.Timestamp}

// zonedTimestamp writes v, a TIMESTAMP in a default form with no zone, as
// a time in loc; ok is false for any other v, which is sent as it was.
func zonedTimestamp(v string, loc *time.Location) (string, bool) {
	m := defaultTimestamp.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil || strings.TrimSpace(m[8]) != "" {
		return "", false
	}
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	nsec := 0
	if m[7] != "" {
		nsec = num((m[7] + "000000")[:6]) * 1000
	}
	y, mo, d, h, mi, s := num(m[1]), num(m[2]), num(m[3]), num(m[4]), num(m[5]), num(m[6])
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || s > 59 {
		return "", false
	}
	tm := time.Date(y, time.Month(mo), d, h, mi, s, nsec, loc)
	if tm.Day() != d {
		return "", false // a day the month does not have
	}
	return tm.UTC().Format(emulatorTimestamp), true
}

// wideReader decodes data, in encoding (UTF-16BE, UTF-16LE, UTF-32BE or
// UTF-32LE), to UTF-8. A byte-order mark at the start is dropped; an
// unpaired surrogate, a value outside Unicode and a last unit cut short
// are read as U+FFFD.
func wideReader(data io.Reader, encoding string) io.Reader {
	w := &wide{r: bufio.NewReader(data), size: 2, order: binary.ByteOrder(binary.BigEndian), start: true}
	if strings.HasPrefix(encoding, "UTF-32") {
		w.size = 4
	}
	if strings.HasSuffix(encoding, "LE") {
		w.order = binary.LittleEndian
	}
	return w
}

type wide struct {
	r       *bufio.Reader
	size    int
	order   binary.ByteOrder
	start   bool
	pending []byte
	unit    [4]byte
	held    rune // a high surrogate waiting for its low one, or 0
	eof     bool
}

func (w *wide) next() (uint32, bool, error) {
	n, err := io.ReadFull(w.r, w.unit[:w.size])
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) && n > 0 {
			return utf8.RuneError, true, nil
		}
		return 0, false, err
	}
	if w.size == 2 {
		return uint32(w.order.Uint16(w.unit[:2])), true, nil
	}
	return w.order.Uint32(w.unit[:4]), true, nil
}

func (w *wide) Read(p []byte) (int, error) {
	for len(w.pending) == 0 {
		if w.eof {
			return 0, io.EOF
		}
		u, ok, err := w.next()
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if !ok {
			w.eof = true
			if w.held > 0 {
				w.pending = utf8.AppendRune(nil, utf8.RuneError)
				w.held = 0
			}
			continue
		}
		first := w.start
		w.start = false
		if first && u == 0xFEFF {
			continue
		}
		var out []byte
		if w.size == 2 {
			r := rune(u)
			switch {
			case w.held > 0 && utf16.IsSurrogate(r) && r >= 0xDC00:
				out = utf8.AppendRune(out, utf16.DecodeRune(w.held, r))
				w.held = 0
			default:
				if w.held > 0 {
					out = utf8.AppendRune(out, utf8.RuneError)
					w.held = 0
				}
				switch {
				case r >= 0xD800 && r < 0xDC00:
					w.held = r
				case utf16.IsSurrogate(r):
					out = utf8.AppendRune(out, utf8.RuneError)
				default:
					out = utf8.AppendRune(out, r)
				}
			}
		} else {
			r := rune(u)
			if u > utf8.MaxRune || utf16.IsSurrogate(r) {
				r = utf8.RuneError
			}
			out = utf8.AppendRune(out, r)
		}
		w.pending = out
	}
	n := copy(p, w.pending)
	w.pending = w.pending[n:]
	return n, nil
}
