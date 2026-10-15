package castformat

import "testing"
import "time"

func TestParseDatetime(t *testing.T) {
	now := time.Date(2021, 3, 23, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in, format string
		k          Kind
		want       string
	}{
		{"03-12-2018", "DD-MM-YYYY", Date, "2018-12-03"},
		{"10000-1203", "YYYY-MMDD", Date, "10000-12-03"},
		{"18", "YYYY", Date, "2018-03-01"},
		{"018-12-03", "YYY-MM-DD", Date, "2018-12-03"},
		{"18-12-03", "YY-MM-DD", Date, "2018-12-03"},
		{"8-12-03", "Y-MM-DD", Date, "2028-12-03"},
		{"2,018-12-03", "Y,YYY-MM-DD", Date, "2018-12-03"},
		{"18-12-03", "RR-MM-DD", Date, "2018-12-03"},
		{"50-12-03", "RR-MM-DD", Date, "1950-12-03"},
		{"DEC 03, 2018", "MON DD, YYYY", Date, "2018-12-03"},
		{"DECEMBER 03, 2018", "MONTH DD, YYYY", Date, "2018-12-03"},
		{"december   03, 2018", "MONTH DD, YYYY", Date, "2018-12-03"},
		{"2018-034", "YYYY-DDD", Date, "2018-02-03"},
		{"03:30 P.M.", "HH:MI P.M.", Time, "15:30:00"},
		{"03:30 A.M.", "HH:MI P.M.", Time, "03:30:00"},
		{"03:30 p.m.", "HH:MI A.M.", Time, "15:30:00"},
		{"12:30 AM", "HH12:MI AM", Time, "00:30:00"},
		{"15:30", "HH24:MI", Time, "15:30:00"},
		{"03:30:02 P.M.", "HH:MI:SS P.M.", Time, "15:30:02"},
		{"03723", "SSSSS", Time, "01:02:03"},
		{"01:05:07.16", "HH24:MI:SS.FF1", Time, "01:05:07.200000"},
		{"01:05:07.16", "HH24:MI:SS.FF2", Time, "01:05:07.160000"},
		{"2008-12-25 05:30:00-08", "YYYY-MM-DD HH24:MI:SSTZH", Timestamp, "2008-12-25 13:30:00.000000"},
		{"2008-12-25 05:30:00+0530", "YYYY-MM-DD HH24:MI:SSTZHTZM", Timestamp, "2008-12-25 00:00:00.000000"},
		{"2020.06.03 00:00:53+00", "YYYY.MM.DD HH24:MI:SSTZH", Timestamp, "2020-06-03 00:00:53.000000"},
		{"2020-06-03 10:11:12", "YYYY-MM-DD HH24:MI:SS", Datetime, "2020-06-03 10:11:12.000000"},
		{"year 2020", `"year" YYYY`, Date, "2020-03-01"},
	} {
		got, err := ParseDatetime(c.in, c.k, c.format, nil, now)
		if err != nil {
			t.Errorf("ParseDatetime(%q, %s, %q): %v", c.in, c.k, c.format, err)
			continue
		}
		var s string
		switch c.k {
		case Date:
			s = got.Format("2006-01-02")
		case Time:
			s = got.Format("15:04:05")
			if got.Nanosecond() != 0 {
				s = got.Format("15:04:05.000000")
			}
		default:
			s = got.UTC().Format("2006-01-02 15:04:05.000000")
		}
		if s != c.want {
			t.Errorf("ParseDatetime(%q, %s, %q) = %s, want %s", c.in, c.k, c.format, s, c.want)
		}
	}
	ny, _ := time.LoadLocation("America/New_York")
	got, err := ParseDatetime("2020-01-01 00:00", Timestamp, "YYYY-MM-DD HH24:MI", ny, now)
	if err != nil || !got.Equal(time.Date(2020, 1, 1, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("AT TIME ZONE America/New_York: %v, %v", got, err)
	}
	for _, c := range []struct {
		in, format string
		k          Kind
	}{
		{"2018-12-03", "YYYY-MM-DD HH24", Date},
		{"12:00", "HH:MI", Time},
		{"12:00 AM", "HH24:MI AM", Time},
		{"034-12", "DDD-MM", Date},
		{"2018-2018", "YYYY-YYYY", Date},
		{"2018-02-30", "YYYY-MM-DD", Date},
		{"2018-02-03x", "YYYY-MM-DD", Date},
		{"2018/02/03", "YYYY-MM-DD", Date},
		{"12:00", "HH24:MI", Date},
	} {
		if _, err := ParseDatetime(c.in, c.k, c.format, nil, now); err == nil {
			t.Errorf("ParseDatetime(%q, %s, %q): want an error", c.in, c.k, c.format)
		}
	}
}
