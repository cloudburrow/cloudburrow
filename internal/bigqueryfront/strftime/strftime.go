package strftime

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// SafeInt converts v to int, failing outside the INT32 range (the
// engine's helper, intconv.go).
func SafeInt(v int64) (int, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("integer value %d is out of the supported range [%d, %d]", v, math.MinInt32, math.MaxInt32)
	}
	return int(v), nil
}

// Kind is the type a value is read as.
type Kind = TimeFormatType

const (
	Date      = FormatTypeDate
	Datetime  = FormatTypeDatetime
	Time      = FormatTypeTime
	Timestamp = FormatTypeTimestamp
)

// IsCStyle reports whether format is C-style: it holds a format element,
// which begins with %. The SQL-style elements (YYYY, MM, ...) have none.
func IsCStyle(format string) bool { return strings.Contains(format, "%") }

// Validate checks that each element of format is one kind can be read
// with.
func Validate(format string, kind Kind) error {
	f := []rune(format)
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		if i >= len(f) {
			return fmt.Errorf("the format ends with %%")
		}
		if f[i] == 'E' {
			if i+1 >= len(f) {
				return fmt.Errorf("the format ends with %%E")
			}
			info, n, err := combinationPatternInfo(f[i+1:])
			if err != nil {
				return err
			}
			if !info.Available(kind) {
				return fmt.Errorf("%%E%s is not an element of a %s format", string(f[i+1:i+1+n]), kind)
			}
			i += n
			continue
		}
		info := formatPatternMap[f[i]]
		if info == nil {
			return fmt.Errorf("%%%c is not a format element", f[i])
		}
		if !info.Available(kind) {
			return fmt.Errorf("%%%c is not an element of a %s format", f[i], kind)
		}
	}
	return nil
}

// zoned reports whether format reads a zone or an absolute time: %z, %Z,
// %Ez or %s.
func zoned(format string) bool {
	for _, e := range []string{"%z", "%Z", "%Ez", "%s"} {
		if strings.Contains(format, e) {
			return true
		}
	}
	return false
}

// Parse reads v in format as kind. A TIMESTAMP whose format reads no zone
// is in loc; the time returned for one is in UTC.
func Parse(format, v string, kind Kind, loc *time.Location) (time.Time, error) {
	t, err := ParseTimeFormat(format, v, kind)
	if err != nil {
		return time.Time{}, err
	}
	got := *t
	if kind == Timestamp {
		if !zoned(format) {
			got = time.Date(got.Year(), got.Month(), got.Day(), got.Hour(), got.Minute(), got.Second(), got.Nanosecond(), loc)
		}
		got = got.UTC()
	}
	return got, nil
}
