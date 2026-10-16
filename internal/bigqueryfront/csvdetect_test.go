package bigqueryfront

import (
	"strings"
	"testing"
)

// With autodetect and a format (#1338), a column whose values the format
// parses is of its type, and one in the type's default form is a STRING;
// timeZone alone leaves the types as a JSON string's.
func TestDetectCSVSchema(t *testing.T) {
	for _, c := range []struct {
		name string
		o    csvOptions
		data string
		want string // name:TYPE ..., or "" for 501
	}{
		{"dateFormat", csvOptions{DateFormat: "%m/%d/%Y"}, "d,iso,n,f,s\n12/03/2018,2018-12-03,1,1.5,x\n01/02/2020,,2,3,y\n",
			"d:DATE iso:STRING n:INTEGER f:FLOAT s:STRING"},
		{"SQL-style", csvOptions{TimestampFormat: "YYYY/MM/DD HH24:MI"}, "ts,b\n2020/07/01 12:00,true\n",
			"ts:TIMESTAMP b:BOOLEAN"},
		{"timeZone", csvOptions{TimeZone: "Asia/Tokyo"}, "ts,d\n2020-07-01 12:00:00,2020-01-02\n",
			"ts:TIMESTAMP d:DATE"},
		{"mixed", csvOptions{DateFormat: "%m/%d/%Y"}, "d\n12/03/2018\ntrue\n", ""},
		{"no header", csvOptions{DateFormat: "%m/%d/%Y"}, "12/03/2018\n01/02/2020\n", ""},
		{"all strings", csvOptions{DateFormat: "%m/%d/%Y"}, "a\nx\n", ""},
		{"ragged", csvOptions{DateFormat: "%m/%d/%Y"}, "a,b\n12/03/2018\n", ""},
	} {
		times, code, msg := csvTimesOf(c.o, false)
		if code != 0 {
			t.Fatalf("%s: %s", c.name, msg)
		}
		fields, why := detectCSVSchema([]byte(c.data), ',', times)
		var parts []string
		for _, f := range fields {
			parts = append(parts, f.Name+":"+f.Type)
		}
		if got := strings.Join(parts, " "); got != c.want || (c.want == "") != (why != "") {
			t.Errorf("%s: %q %q, want %q", c.name, got, why, c.want)
		}
	}
}

// TestDetectCSVHeader (#1457): skipLeadingRows with autodetect as
// JobConfigurationLoad documents it, and generic names when no header is
// detected.
func TestDetectCSVHeader(t *testing.T) {
	times := &csvTimes{formats: map[string]string{}}
	for _, c := range []struct {
		name, data string
		skip       int64
		skipSet    bool
		want       string // name:TYPE ..., or "" for 501
		send       int64
		header     bool
	}{
		{"unset, a header", "name,n\nbob,3\n", 0, false, "name:STRING n:INTEGER", 1, true},
		{"unset, all strings: the row is data", "a,b\nx,y\n", 0, false, "string_field_0:STRING string_field_1:STRING", 0, false},
		{"unset, a typed first row is data", "1,x\n2,y\n", 0, false, "int64_field_0:INTEGER string_field_1:STRING", 0, false},
		{"0: no header", "name,n\nbob,3\n", 0, true, "string_field_0:STRING string_field_1:STRING", 0, false},
		{"0: no header, typed", "1,2.5,true\n2,3,false\n", 0, true, "int64_field_0:INTEGER double_field_1:FLOAT bool_field_2:BOOLEAN", 0, false},
		{"3: the header is row 3", "title\nnote\nname,n\nbob,3\n", 3, true, "name:STRING n:INTEGER", 3, true},
		{"2: row 2 is no header, and is skipped", "junk\n1,x\n2,y\n", 2, true, "int64_field_0:INTEGER string_field_1:STRING", 2, false},
		{"1: all strings, row 1 skipped", "a,b\nx,y\n", 1, true, "string_field_0:STRING string_field_1:STRING", 1, false},
		{"rows of different widths", "a,b\n1\n", 0, false, "", 0, false},
		{"nothing after the skipped rows", "a\n", 2, true, "", 0, false},
	} {
		fields, send, header, why := detectCSVHeader([]byte(c.data), ',', times, c.skip, c.skipSet)
		var parts []string
		for _, f := range fields {
			parts = append(parts, f.Name+":"+f.Type)
		}
		if got := strings.Join(parts, " "); got != c.want || (c.want == "") != (why != "") ||
			c.want != "" && (send != c.send || header != c.header) {
			t.Errorf("%s: %q send %d header %v %q, want %q %d %v", c.name, got, send, header, why, c.want, c.send, c.header)
		}
	}
}
