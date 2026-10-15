package bigqueryfront

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestCSVTimesConvert(t *testing.T) {
	times, code, msg := csvTimesOf(csvOptions{TimeZone: "America/New_York", DateFormat: "MM/DD/YYYY",
		DatetimeFormat: "DD.MM.YYYY HH24:MI", TimeFormat: "HH12:MI P.M."}, false)
	if code != 0 {
		t.Fatal(msg)
	}
	cols := []field{{Name: "d", Type: "DATE"}, {Name: "dt", Type: "DATETIME"}, {Name: "t", Type: "TIME"},
		{Name: "ts", Type: "TIMESTAMP"}, {Name: "z", Type: "TIMESTAMP"}, {Name: "n", Type: "INTEGER"}}
	rec := []string{"12/03/2018", "03.12.2018 15:30", "03:30 P.M.", "2020-07-01 12:00:00", "2020-07-01 12:00:00+00:00", "7"}
	if bad := times.convert(rec, cols); bad != "" {
		t.Fatal(bad)
	}
	want := "2018-12-03|2018-12-03 15:30:00|15:30:00|2020-07-01 16:00:00+00:00|2020-07-01 12:00:00+00:00|7"
	if got := strings.Join(rec, "|"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if bad := times.convert([]string{"2018-12-03"}, cols); bad == "" {
		t.Error("a DATE outside its format was not refused")
	}
	for format, want := range map[string]string{
		"YYYY-MM-DD HH24:MI:SSTZH": "2008-12-25 13:30:00+00:00", // its own zone
		"YYYY-MM-DD HH24:MI:SS":    "2008-12-25 00:00:00+00:00", // timeZone's
	} {
		stamp, _, _ := csvTimesOf(csvOptions{TimestampFormat: format, TimeZone: "+05:30"}, false)
		rec = []string{"2008-12-25 05:30:00-08"}
		if !strings.HasSuffix(format, "TZH") {
			rec[0] = "2008-12-25 05:30:00"
		}
		if bad := stamp.convert(rec, []field{{Name: "a", Type: "TIMESTAMP"}}); bad != "" || rec[0] != want {
			t.Errorf("timestampFormat %s: got %s %s, want %s", format, rec[0], bad, want)
		}
	}
}

func TestWideReader(t *testing.T) {
	text := "\ufeffid,name\n1,\u00e9\U0001F600\n"
	u := utf16.Encode([]rune(text))
	be, le := &bytes.Buffer{}, &bytes.Buffer{}
	for _, x := range u {
		be.Write([]byte{byte(x >> 8), byte(x)})
		le.Write([]byte{byte(x), byte(x >> 8)})
	}
	b32 := &bytes.Buffer{}
	for _, r := range text {
		b32.Write([]byte{byte(r), byte(r >> 8), byte(r >> 16), byte(r >> 24)})
	}
	for enc, data := range map[string][]byte{"UTF-16BE": be.Bytes(), "UTF-16LE": le.Bytes(), "UTF-32LE": b32.Bytes()} {
		got, err := io.ReadAll(wideReader(bytes.NewReader(data), enc))
		if err != nil || string(got) != text[3:] {
			t.Errorf("%s: %q %v", enc, got, err)
		}
	}
	got, _ := io.ReadAll(wideReader(bytes.NewReader([]byte{0xD8, 0x00, 0x00, 0x41}), "UTF-16BE"))
	if string(got) != "�A" {
		t.Errorf("an unpaired surrogate: %q", got)
	}
}
