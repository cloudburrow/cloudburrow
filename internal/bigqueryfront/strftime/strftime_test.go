package strftime

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	for _, c := range []struct {
		format, v string
		kind      Kind
		loc       *time.Location
		want      string // RFC 3339, or "" for an error
	}{
		{"%m/%d/%Y", "01/02/2020", Date, time.UTC, "2020-01-02T00:00:00Z"},
		{"%m/%d/%Y", "01-02-2020", Date, time.UTC, ""},
		{"%m/%d/%Y", "13/02/2020", Date, time.UTC, ""},
		{"%d.%m.%Y %H:%M:%S", "02.01.2020 03:04:05", Datetime, time.UTC, "2020-01-02T03:04:05Z"},
		{"%H.%M", "03.04", Time, time.UTC, "1970-01-01T03:04:00Z"},
		{"%Y/%m/%d %H:%M", "2020/01/02 03:04", Timestamp, ny, "2020-01-02T08:04:00Z"},
		{"%Y/%m/%d %H:%M %Ez", "2020/01/02 03:04 +01:00", Timestamp, ny, "2020-01-02T02:04:00Z"},
	} {
		got, err := Parse(c.format, c.v, c.kind, c.loc)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("Parse(%q, %q) = %v, want an error", c.format, c.v, got)
		case c.want != "" && (err != nil || got.Format(time.RFC3339) != c.want):
			t.Errorf("Parse(%q, %q) = %v %v, want %s", c.format, c.v, got, err, c.want)
		}
	}
	for format, ok := range map[string]bool{"%m/%d/%Y": true, "%H": false, "%Q%": false, "%k": false} {
		if err := Validate(format, Date); (err == nil) != ok {
			t.Errorf("Validate(%q, DATE): %v", format, err)
		}
	}
}
