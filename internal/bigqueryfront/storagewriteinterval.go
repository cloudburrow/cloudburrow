package bigqueryfront

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// INTERVAL values written through the Storage Write API's proto_rows
// (#1424). The supported-data-types page's table gives an INTERVAL column
// two protocol buffer types, string and google.protobuf.Duration, and says
// "The string value must be an INTERVAL literal". A string is read here in
// BigQuery's canonical format, "[sign]Y-M [sign]D [sign]H:M:S[.F]" (what
// tabledata.list, jobs.query and CAST(... AS STRING) give), or as the
// literal's own syntax: INTERVAL 5 DAY, or INTERVAL '1-2' YEAR TO MONTH.
// Either is written to the table in the canonical format, its parts in
// BigQuery's ranges. A Duration is its seconds and nanoseconds, written as
// the time part: 90061.5 seconds is 0-0 0 25:1:1.5. A value that is not an
// interval, or is out of INTERVAL's range, fails its row (a row error, as
// any value that does not convert).

// INTERVAL's range (data-types#interval_type): -10000-0 -3660000
// -87840000:0:0 to 10000-0 3660000 87840000:0:0.
const (
	intervalMaxMonths = 10000 * 12
	intervalMaxDays   = 3660000
	intervalMaxMicros = int64(87840000) * 3600 * 1_000_000
)

// interval is an INTERVAL's three parts; micros is the time part.
type interval struct{ months, days, micros int64 }

func (iv interval) check() error {
	if abs64(iv.months) > intervalMaxMonths || abs64(iv.days) > intervalMaxDays || abs64(iv.micros) > intervalMaxMicros {
		return fmt.Errorf("the INTERVAL is out of range: an INTERVAL is from -10000-0 -3660000 -87840000:0:0 to 10000-0 3660000 87840000:0:0")
	}
	return nil
}

// text is the canonical format, as intervalText writes it; the time part
// is given in microseconds, since one in nanoseconds can pass INT64.
func (iv interval) text() string {
	t := intervalText(iv.months, iv.days, 0)
	t = t[:strings.LastIndexByte(t, ' ')+1]
	us, sign := iv.micros, ""
	if us < 0 {
		sign, us = "-", -us
	}
	secs := us / 1_000_000
	t += fmt.Sprintf("%s%d:%d:%d", sign, secs/3600, secs/60%60, secs%60)
	if f := us % 1_000_000; f != 0 {
		t += strings.TrimRight(fmt.Sprintf(".%06d", f), "0")
	}
	return t
}

// satMul is n*f, f positive, held past INTERVAL's range rather than
// overflowing, so that the range check refuses it.
func satMul(n, f int64) int64 {
	if lim := (intervalMaxMicros + 1) / f; n > lim {
		return intervalMaxMicros + 1
	} else if n < -lim {
		return -intervalMaxMicros - 1
	}
	return n * f
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// The canonical format; a fraction of up to nine digits is read, and what
// is below a microsecond, which an INTERVAL does not hold, is cut.
var canonicalInterval = regexp.MustCompile(`^([-+]?)(\d+)-(\d+) ([-+]?\d+) ([-+]?)(\d+):(\d+):(\d+)(?:\.(\d{1,9}))?$`)

// intervalLiteralPart: INTERVAL n PART.
var intervalLiteralPart = regexp.MustCompile(`(?i)^INTERVAL\s+([-+]?\d+)\s+([A-Z]+)$`)

// intervalLiteralRange: INTERVAL 'parts' FROM TO PART.
var intervalLiteralRange = regexp.MustCompile(`(?i)^INTERVAL\s+(?:'([^']*)'|"([^"]*)")\s+([A-Z]+)\s+TO\s+([A-Z]+)$`)

// writeIntervalText reads s, a string written to an INTERVAL column, as
// its canonical text.
func writeIntervalText(s string) (string, error) {
	iv, err := parseIntervalString(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	if err := iv.check(); err != nil {
		return "", err
	}
	return iv.text(), nil
}

func parseIntervalString(s string) (interval, error) {
	bad := fmt.Errorf("%q is not an INTERVAL: write one in the canonical format [sign]Y-M [sign]D [sign]H:M:S[.F] "+
		"(for example 1-2 3 4:5:6.789) or as an INTERVAL literal", s)
	if m := canonicalInterval.FindStringSubmatch(s); m != nil {
		n := make([]int64, 0, 6)
		for _, p := range []string{m[2], m[3], m[4], m[6], m[7], m[8]} {
			v, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return interval{}, bad
			}
			n = append(n, v)
		}
		y, mo, d, h, mi, sec := n[0], n[1], n[2], n[3], n[4], n[5]
		if mo > 11 || mi > 59 || sec > 59 || y > intervalMaxMonths || h > 87840000 {
			return interval{}, bad
		}
		iv := interval{months: y*12 + mo, days: d, micros: ((h*60+mi)*60+sec)*1_000_000 + fractionMicros(m[9])}
		if m[1] == "-" {
			iv.months = -iv.months
		}
		if m[5] == "-" {
			iv.micros = -iv.micros
		}
		return iv, nil
	}
	if m := intervalLiteralPart.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || abs64(n) > intervalMaxMicros {
			return interval{}, bad
		}
		switch strings.ToUpper(m[2]) {
		case "YEAR":
			return interval{months: satMul(n, 12)}, nil
		case "QUARTER":
			return interval{months: satMul(n, 3)}, nil
		case "MONTH":
			return interval{months: n}, nil
		case "WEEK":
			return interval{days: satMul(n, 7)}, nil
		case "DAY":
			return interval{days: n}, nil
		case "HOUR":
			return interval{micros: satMul(n, 3600*1_000_000)}, nil
		case "MINUTE":
			return interval{micros: satMul(n, 60*1_000_000)}, nil
		case "SECOND":
			return interval{micros: satMul(n, 1_000_000)}, nil
		case "MILLISECOND":
			return interval{micros: satMul(n, 1000)}, nil
		case "MICROSECOND":
			return interval{micros: n}, nil
		}
		return interval{}, bad
	}
	if m := intervalLiteralRange.FindStringSubmatch(s); m != nil {
		text := m[1] + m[2]
		iv, ok := parseIntervalRange(strings.TrimSpace(text), strings.ToUpper(m[3]), strings.ToUpper(m[4]))
		if !ok {
			return interval{}, bad
		}
		return iv, nil
	}
	return interval{}, bad
}

// intervalRangeParts are the datetime parts of the range syntax, in order,
// each with the separator that comes before it.
var intervalRangeParts = []struct{ name, sep string }{
	{"YEAR", ""}, {"MONTH", "-"}, {"DAY", " "}, {"HOUR", " "}, {"MINUTE", ":"}, {"SECOND", ":"},
}

// parseIntervalRange reads text, the parts from one datetime part to
// another ('1-2' YEAR TO MONTH, '10:20:30.52' HOUR TO SECOND). A sign
// before the year or month distributes over the years and months; one
// before the hour, minute or second over the time part.
func parseIntervalRange(text, from, to string) (interval, bool) {
	first, last := -1, -1
	for i, p := range intervalRangeParts {
		if p.name == from {
			first = i
		}
		if p.name == to {
			last = i
		}
	}
	if first < 0 || last <= first {
		return interval{}, false
	}
	// The pattern of the parts from first to last, each a signed number
	// where a sign may stand (the first of the Y-M part, the day, and the
	// first of the time part); the seconds may have a fraction.
	var pat strings.Builder
	pat.WriteString("^")
	for i := first; i <= last; i++ {
		p := intervalRangeParts[i]
		if i > first {
			pat.WriteString(regexp.QuoteMeta(p.sep))
		}
		signed := i == first || p.name == "DAY" || p.name == "HOUR"
		if signed {
			pat.WriteString(`([-+]?\d+)`)
		} else {
			pat.WriteString(`(\d+)`)
		}
		if p.name == "SECOND" {
			pat.WriteString(`(?:\.(\d{1,9}))?`)
		}
	}
	pat.WriteString("$")
	m := regexp.MustCompile(pat.String()).FindStringSubmatch(text)
	if m == nil {
		return interval{}, false
	}
	var iv interval
	var ymNeg, timeNeg bool
	idx := 1
	for i := first; i <= last; i++ {
		p := intervalRangeParts[i]
		v, err := strconv.ParseInt(m[idx], 10, 64)
		idx++
		if err != nil || abs64(v) > intervalMaxMicros {
			return interval{}, false
		}
		neg := strings.HasPrefix(m[idx-1], "-")
		switch p.name {
		case "YEAR":
			ymNeg = neg
			iv.months += satMul(abs64(v), 12)
		case "MONTH":
			if i == first {
				ymNeg = neg
			} else if v > 11 {
				return interval{}, false
			}
			iv.months += abs64(v)
		case "DAY":
			iv.days = v
		case "HOUR":
			timeNeg = neg
			iv.micros += satMul(abs64(v), 3600*1_000_000)
		case "MINUTE":
			if i == first {
				timeNeg = neg
			} else if v > 59 {
				return interval{}, false
			}
			iv.micros += satMul(abs64(v), 60*1_000_000)
		case "SECOND":
			if i == first {
				timeNeg = neg
			} else if v > 59 {
				return interval{}, false
			}
			iv.micros += satMul(abs64(v), 1_000_000) + fractionMicros(m[idx])
			idx++
		}
	}
	if ymNeg {
		iv.months = -iv.months
	}
	if timeNeg {
		iv.micros = -iv.micros
	}
	return iv, true
}

// fractionMicros is the fraction of a second f, its digits, in whole
// microseconds (the digits below a microsecond cut).
func fractionMicros(f string) int64 {
	if f == "" {
		return 0
	}
	f = (f + "000000")[:6]
	us, _ := strconv.ParseInt(f, 10, 64)
	return us
}

// durationIntervalText writes a google.protobuf.Duration, seconds and
// nanos, as an INTERVAL's canonical text: the time part, below a
// microsecond cut. A Duration that is not valid (its nanos past a second,
// or of the other sign than its seconds) or out of INTERVAL's range fails.
func durationIntervalText(secs, nanos int64) (string, error) {
	if nanos <= -1e9 || nanos >= 1e9 || (secs > 0 && nanos < 0) || (secs < 0 && nanos > 0) {
		return "", fmt.Errorf("the google.protobuf.Duration of %d seconds and %d nanos is not valid: its nanos must be of its seconds' sign and under a second", secs, nanos)
	}
	if abs64(secs) > intervalMaxMicros/1_000_000 || secs == math.MinInt64 {
		return "", interval{micros: intervalMaxMicros + 1}.check()
	}
	iv := interval{micros: secs*1_000_000 + nanos/1000}
	if err := iv.check(); err != nil {
		return "", err
	}
	return iv.text(), nil
}
