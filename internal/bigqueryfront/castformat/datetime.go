// Package castformat implements the FORMAT clause of CAST: BigQuery's
// format elements (cloud.google.com/bigquery/docs/reference/standard-sql/
// format-elements) for BYTES to and from STRING, DATE, DATETIME, TIME and
// TIMESTAMP to and from STRING, and numeric types to STRING.
//
// CloudBurrow patch (cloudburrow/cloudburrow#1194, #1195).
//
// This copy is the DATE, DATETIME, TIME and TIMESTAMP part of the
// emulator's engine patch, third_party/bigquery-emulator/patches/
// googlesqlite/0023-cast-format.patch (internal/castformat/datetime.go),
// for the front's CSV loads with dateFormat, datetimeFormat, timeFormat and
// timestampFormat (#1325), which BigQuery reads with the same format
// elements. Keep the two the same.
package castformat

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Kind is the date and time type on one side of a cast.
type Kind int

const (
	Date Kind = iota
	Datetime
	Time
	Timestamp
)

func (k Kind) String() string {
	switch k {
	case Date:
		return "DATE"
	case Datetime:
		return "DATETIME"
	case Time:
		return "TIME"
	}
	return "TIMESTAMP"
}

func (k Kind) hasDate() bool { return k != Time }
func (k Kind) hasTime() bool { return k != Date }

type elemKind int

const (
	eLiteral elemKind = iota
	eSpace
	eYYYY
	eYYY
	eYY
	eY
	eYcommaYYY
	eRRRR
	eRR
	eMM
	eMON
	eMONTH
	eDAY
	eDY
	eD
	eDD
	eDDD
	eHH
	eHH12
	eHH24
	eMI
	eSS
	eSSSSS
	eFF
	eAMdot // A.M. or P.M.
	eAM    // AM or PM
	eTZH
	eTZM
)

type elem struct {
	kind elemKind
	text string // the element as written, or the literal text
	n    int    // FFn digits
}

// keywords, longest first where one is a prefix of another.
var dtKeywords = []struct {
	word string
	kind elemKind
}{
	{"Y,YYY", eYcommaYYY},
	{"YYYY", eYYYY}, {"YYY", eYYY}, {"YY", eYY}, {"Y", eY},
	{"RRRR", eRRRR}, {"RR", eRR},
	{"MONTH", eMONTH}, {"MON", eMON}, {"MM", eMM}, {"MI", eMI},
	{"DAY", eDAY}, {"DDD", eDDD}, {"DD", eDD}, {"DY", eDY}, {"D", eD},
	{"HH24", eHH24}, {"HH12", eHH12}, {"HH", eHH},
	{"SSSSS", eSSSSS}, {"SS", eSS},
	{"A.M.", eAMdot}, {"P.M.", eAMdot}, {"AM", eAM}, {"PM", eAM},
	{"TZH", eTZH}, {"TZM", eTZM},
}

func parseDatetimeFormat(f string) ([]elem, error) {
	var out []elem
	for i := 0; i < len(f); {
		c := f[i]
		switch {
		case c == ' ':
			j := i
			for j < len(f) && f[j] == ' ' {
				j++
			}
			out = append(out, elem{kind: eSpace, text: f[i:j]})
			i = j
			continue
		case strings.IndexByte("-./,';:", c) >= 0:
			// "." starts "A.M." only after A or P, handled below; a
			// literal "," never starts "Y,YYY".
			out = append(out, elem{kind: eLiteral, text: string(c)})
			i++
			continue
		case c == '"':
			var sb strings.Builder
			j := i + 1
			for ; j < len(f) && f[j] != '"'; j++ {
				if f[j] == '\\' {
					if j+1 < len(f) && (f[j+1] == '"' || f[j+1] == '\\') {
						j++
					} else {
						return nil, fmt.Errorf("unsupported escape sequence in format string %q", f)
					}
				}
				sb.WriteByte(f[j])
			}
			if j >= len(f) {
				return nil, fmt.Errorf("unterminated literal in format string %q", f)
			}
			out = append(out, elem{kind: eLiteral, text: sb.String()})
			i = j + 1
			continue
		case unicode.IsSpace(rune(c)):
			return nil, fmt.Errorf("whitespace other than a space is not allowed in format string %q", f)
		}
		up := strings.ToUpper(f[i:])
		if strings.HasPrefix(up, "FF") && len(up) > 2 && up[2] >= '1' && up[2] <= '9' {
			out = append(out, elem{kind: eFF, text: f[i : i+3], n: int(up[2] - '0')})
			i += 3
			continue
		}
		matched := false
		for _, k := range dtKeywords {
			if strings.HasPrefix(up, k.word) {
				out = append(out, elem{kind: k.kind, text: f[i : i+len(k.word)]})
				i += len(k.word)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("unsupported format element %q in format string %q", f[i:], f)
		}
	}
	return out, nil
}

func needsDate(k elemKind) bool {
	switch k {
	case eYYYY, eYYY, eYY, eY, eYcommaYYY, eRRRR, eRR, eMM, eMON, eMONTH, eDAY, eDY, eD, eDD, eDDD:
		return true
	}
	return false
}

func needsTime(k elemKind) bool {
	switch k {
	case eHH, eHH12, eHH24, eMI, eSS, eSSSSS, eFF, eAMdot, eAM:
		return true
	}
	return false
}

// caseMatch writes word (upper case) in the letter case of the element.
func caseMatch(elemText, word string) string {
	if len(elemText) >= 2 && unicode.IsUpper(rune(elemText[0])) && unicode.IsUpper(rune(elemText[1])) {
		return strings.ToUpper(word)
	}
	if len(elemText) >= 1 && unicode.IsUpper(rune(elemText[0])) {
		return strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
	}
	return strings.ToLower(word)
}

func pad(n, width int) string {
	s := strconv.Itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// FormatDatetime formats t, a value of kind k, as a string. A TIMESTAMP is
// written in loc.
func FormatDatetime(t time.Time, k Kind, format string, loc *time.Location) (string, error) {
	elems, err := parseDatetimeFormat(format)
	if err != nil {
		return "", err
	}
	if k == Timestamp {
		if loc == nil {
			loc = time.UTC
		}
		t = t.In(loc)
	}
	var sb strings.Builder
	for _, e := range elems {
		if needsDate(e.kind) && !k.hasDate() {
			return "", fmt.Errorf("format element %q is not allowed for %s", e.text, k)
		}
		if needsTime(e.kind) && !k.hasTime() {
			return "", fmt.Errorf("format element %q is not allowed for %s", e.text, k)
		}
		y := t.Year()
		ay := y
		if ay < 0 {
			ay = -ay
		}
		switch e.kind {
		case eLiteral, eSpace:
			sb.WriteString(e.text)
		case eYYYY, eRRRR:
			sb.WriteString(pad(y, 4))
		case eYcommaYYY:
			sb.WriteString(strconv.Itoa(ay/1000) + "," + pad(ay%1000, 3))
		case eYYY:
			sb.WriteString(pad(ay%1000, 3))
		case eYY, eRR:
			sb.WriteString(pad(ay%100, 2))
		case eY:
			sb.WriteString(strconv.Itoa(ay % 10))
		case eMM:
			sb.WriteString(pad(int(t.Month()), 2))
		case eMON:
			sb.WriteString(caseMatch(e.text, t.Month().String()[:3]))
		case eMONTH:
			sb.WriteString(caseMatch(e.text, t.Month().String()))
		case eDAY:
			sb.WriteString(caseMatch(e.text, fmt.Sprintf("%-9s", t.Weekday().String())))
		case eDY:
			sb.WriteString(caseMatch(e.text, t.Weekday().String()[:3]))
		case eD:
			sb.WriteString(strconv.Itoa(int(t.Weekday()) + 1))
		case eDD:
			sb.WriteString(pad(t.Day(), 2))
		case eDDD:
			sb.WriteString(pad(t.YearDay(), 3))
		case eHH, eHH12:
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			sb.WriteString(pad(h, 2))
		case eHH24:
			sb.WriteString(pad(t.Hour(), 2))
		case eMI:
			sb.WriteString(pad(t.Minute(), 2))
		case eSS:
			sb.WriteString(pad(t.Second(), 2))
		case eSSSSS:
			sb.WriteString(pad(t.Hour()*3600+t.Minute()*60+t.Second(), 5))
		case eFF:
			sb.WriteString(pad(t.Nanosecond(), 9)[:e.n])
		case eAMdot, eAM:
			word := "AM"
			if t.Hour() >= 12 {
				word = "PM"
			}
			if e.kind == eAMdot {
				word = word[:1] + "." + word[1:] + "."
			}
			if unicode.IsLower(rune(e.text[0])) {
				word = strings.ToLower(word)
			}
			sb.WriteString(word)
		case eTZH, eTZM:
			_, off := t.Zone()
			sign := "+"
			if off < 0 {
				sign = "-"
				off = -off
			}
			if e.kind == eTZH {
				sb.WriteString(sign + pad(off/3600, 2))
			} else {
				sb.WriteString(pad(off%3600/60, 2))
			}
		}
	}
	return sb.String(), nil
}

// ParseDatetime parses s, formatted as format, as a value of kind k. A
// TIMESTAMP with no TZH is read in loc; parts that are missing default to
// the current year and month (in loc), day 1, and midnight.
func ParseDatetime(s string, k Kind, format string, loc *time.Location, now time.Time) (time.Time, error) {
	elems, err := parseDatetimeFormat(format)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	if err := validateParseModel(elems, k); err != nil {
		return time.Time{}, err
	}
	now = now.In(loc)
	year, month, day := now.Year(), int(now.Month()), 1
	yday := 0
	hour, minute, sec, nsec := 0, 0, 0, 0
	hour12, pm, hasMeridian := -1, false, false
	tzSet, tzNeg, tzh, tzm := false, false, 0, 0

	in := strings.TrimSpace(s)
	pos := 0
	errAt := func(e elem) error {
		return fmt.Errorf("could not parse %q with format %q: mismatch at element %q", s, format, e.text)
	}
	// digits reads at most max digits, at least one; exact when the next
	// element also reads digits with no delimiter between.
	digits := func(i, width, max int) (int, int, bool) {
		if i+1 < len(elems) && readsDigits(elems[i+1].kind) {
			max = width
		}
		j := pos
		for j < len(in) && j-pos < max && in[j] >= '0' && in[j] <= '9' {
			j++
		}
		if j == pos {
			return 0, 0, false
		}
		n, _ := strconv.Atoi(in[pos:j])
		w := j - pos
		pos = j
		return n, w, true
	}
	replaceLast := func(n, pow int) int {
		return year - year%pow + n
	}
	for i, e := range elems {
		switch e.kind {
		case eLiteral:
			if !strings.HasPrefix(in[pos:], e.text) {
				return time.Time{}, errAt(e)
			}
			pos += len(e.text)
		case eSpace:
			j := pos
			for j < len(in) {
				r := rune(in[j])
				if !unicode.IsSpace(r) {
					break
				}
				j++
			}
			if j == pos {
				return time.Time{}, errAt(e)
			}
			pos = j
		case eYYYY, eRRRR:
			n, w, ok := digits(i, 4, 5)
			if !ok {
				return time.Time{}, errAt(e)
			}
			if w <= 2 {
				year = replaceLast(n, 100)
			} else {
				year = n
			}
		case eYcommaYYY:
			hi, _, ok := digits(i, 2, 2)
			if !ok || pos >= len(in) || in[pos] != ',' {
				return time.Time{}, errAt(e)
			}
			pos++
			lo, w, ok := digits(i, 3, 3)
			if !ok || w != 3 {
				return time.Time{}, errAt(e)
			}
			year = hi*1000 + lo
		case eYYY:
			n, _, ok := digits(i, 3, 3)
			if !ok {
				return time.Time{}, errAt(e)
			}
			year = replaceLast(n, 1000)
		case eYY:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			year = replaceLast(n, 100)
		case eY:
			n, _, ok := digits(i, 1, 1)
			if !ok {
				return time.Time{}, errAt(e)
			}
			year = replaceLast(n, 10)
		case eRR:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			cur := now.Year()
			century, last := cur/100, cur%100
			switch {
			case n < 50 && last >= 50:
				century++
			case n >= 50 && last < 50:
				century--
			}
			year = century*100 + n
		case eMM:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			month = n
		case eMON, eMONTH:
			m := 0
			for c := time.January; c <= time.December; c++ {
				name := c.String()
				if e.kind == eMON {
					name = name[:3]
				}
				if len(in)-pos >= len(name) && strings.EqualFold(in[pos:pos+len(name)], name) {
					m = int(c)
					pos += len(name)
					break
				}
			}
			if m == 0 {
				return time.Time{}, errAt(e)
			}
			month = m
		case eDD:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			day = n
		case eDDD:
			n, _, ok := digits(i, 3, 3)
			if !ok {
				return time.Time{}, errAt(e)
			}
			yday = n
		case eHH, eHH12:
			n, _, ok := digits(i, 2, 2)
			if !ok || n < 1 || n > 12 {
				return time.Time{}, errAt(e)
			}
			hour12 = n % 12
		case eHH24:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			hour = n
		case eMI:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			minute = n
		case eSS:
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			sec = n
		case eSSSSS:
			n, _, ok := digits(i, 5, 5)
			if !ok || n >= 86400 {
				return time.Time{}, errAt(e)
			}
			hour, minute, sec = n/3600, n%3600/60, n%60
		case eFF:
			j := pos
			for j < len(in) && in[j] >= '0' && in[j] <= '9' {
				j++
			}
			if j == pos {
				return time.Time{}, errAt(e)
			}
			frac := in[pos:j]
			pos = j
			// Round the matched fraction to n digits.
			r, _ := new(big.Rat).SetString("0." + frac)
			scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(e.n)), nil)
			r.Mul(r, new(big.Rat).SetInt(scale))
			q := roundHalfAway(r)
			if q.Cmp(scale) >= 0 {
				q.Sub(q, scale)
				sec++
			}
			nsec = int(q.Int64())
			for k := e.n; k < 9; k++ {
				nsec *= 10
			}
		case eAMdot, eAM:
			want := 2
			if e.kind == eAMdot {
				want = 4
			}
			if len(in)-pos < want {
				return time.Time{}, errAt(e)
			}
			word := strings.ToUpper(in[pos : pos+want])
			switch word {
			case "AM", "A.M.":
			case "PM", "P.M.":
				pm = true
			default:
				return time.Time{}, errAt(e)
			}
			hasMeridian = true
			pos += want
		case eTZH:
			if pos >= len(in) || strings.IndexByte("+- ", in[pos]) < 0 {
				return time.Time{}, errAt(e)
			}
			tzNeg = in[pos] == '-'
			pos++
			n, _, ok := digits(i, 2, 2)
			if !ok {
				return time.Time{}, errAt(e)
			}
			tzh, tzSet = n, true
		case eTZM:
			n, _, ok := digits(i, 2, 2)
			if !ok || n > 59 {
				return time.Time{}, errAt(e)
			}
			tzm, tzSet = n, true
		default:
			return time.Time{}, fmt.Errorf("format element %q is not supported when casting a string to %s", e.text, k)
		}
	}
	if pos != len(in) {
		return time.Time{}, fmt.Errorf("could not parse %q with format %q: trailing characters %q", s, format, in[pos:])
	}
	if hasMeridian {
		hour = hour12
		if pm {
			hour += 12
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || sec > 60 {
		return time.Time{}, fmt.Errorf("could not parse %q with format %q: a part is out of range", s, format)
	}
	if yday > 0 {
		month, day = 1, yday
		if yday > time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC).YearDay() {
			return time.Time{}, fmt.Errorf("could not parse %q with format %q: day of year out of range", s, format)
		}
	} else if time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Day() != day {
		return time.Time{}, fmt.Errorf("could not parse %q with format %q: day out of range for its month", s, format)
	}
	switch k {
	case Date:
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), nil
	case Datetime:
		return time.Date(year, time.Month(month), day, hour, minute, sec, nsec, time.UTC), nil
	case Time:
		return time.Date(0, 1, 1, hour, minute, sec, nsec, time.UTC), nil
	}
	zone := loc
	if tzSet {
		off := tzh*3600 + tzm*60
		if tzNeg {
			off = -off
		}
		zone = time.FixedZone("", off)
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, nsec, zone), nil
}

func readsDigits(k elemKind) bool {
	switch k {
	case eYYYY, eYYY, eYY, eY, eYcommaYYY, eRRRR, eRR, eMM, eDD, eDDD, eHH, eHH12, eHH24, eMI, eSS, eSSSSS, eFF, eTZM:
		return true
	}
	return false
}

// validateParseModel checks the documented format model rules.
func validateParseModel(elems []elem, k Kind) error {
	seen := map[elemKind]string{}
	parts := map[string]string{}
	part := func(e elem) string {
		switch e.kind {
		case eYYYY, eYYY, eYY, eY, eYcommaYYY, eRRRR, eRR:
			return "year"
		case eMM, eMON, eMONTH:
			return "month"
		case eDD, eDDD, eD, eDAY, eDY:
			return "day"
		case eHH, eHH12, eHH24:
			return "hour"
		case eAMdot, eAM:
			return "meridian indicator"
		}
		return ""
	}
	for _, e := range elems {
		if e.kind == eLiteral || e.kind == eSpace {
			continue
		}
		if needsDate(e.kind) && !k.hasDate() || needsTime(e.kind) && !k.hasTime() {
			return fmt.Errorf("format element %q is not allowed when casting a string to %s", e.text, k)
		}
		if _, dup := seen[e.kind]; dup {
			return fmt.Errorf("format element %q appears more than once", e.text)
		}
		seen[e.kind] = e.text
		if p := part(e); p != "" {
			if prev, dup := parts[p]; dup {
				return fmt.Errorf("format elements %q and %q both set the %s", prev, e.text, p)
			}
			parts[p] = e.text
		}
	}
	has := func(ks ...elemKind) bool {
		for _, k := range ks {
			if _, ok := seen[k]; ok {
				return true
			}
		}
		return false
	}
	switch {
	case has(eDDD) && has(eMM, eMON, eMONTH):
		return fmt.Errorf("format element DDD cannot be used with a month element")
	case has(eHH24) && has(eHH, eHH12, eAMdot, eAM):
		return fmt.Errorf("format element HH24 cannot be used with a 12-hour element or a meridian indicator")
	case has(eHH, eHH12) && !has(eAMdot, eAM):
		return fmt.Errorf("a 12-hour format element requires a meridian indicator")
	case has(eAMdot, eAM) && !has(eHH, eHH12):
		return fmt.Errorf("a meridian indicator requires a 12-hour format element")
	case has(eSSSSS) && has(eHH, eHH12, eHH24, eMI, eSS, eAMdot, eAM):
		return fmt.Errorf("format element SSSSS cannot be used with hour, minute, second or meridian elements")
	}
	return nil
}

func roundHalfAway(r *big.Rat) *big.Int {
	num := new(big.Int).Abs(r.Num())
	den := r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if r.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// ValidateParse reports why format cannot parse a value of kind k, or nil.
func ValidateParse(format string, k Kind) error {
	elems, err := parseDatetimeFormat(format)
	if err != nil {
		return err
	}
	return validateParseModel(elems, k)
}
