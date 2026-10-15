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
