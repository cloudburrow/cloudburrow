package datatransfer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront/strftime"
)

// templateParamRE is one {run_time[+-offset]|"format"} parameter, with no
// whitespace inside it, as Google documents: offsets are h, m or s, decimals
// allowed, and days are not supported.
var templateParamRE = regexp.MustCompile(`^run_time(?:([+-])([0-9]+(?:\.[0-9]+)?)([hms]))?\|"([^"]*)"$`)

// RenderTableTemplate renders a destination_table_name_template for one
// run: {run_date} is the run's date as YYYYMMDD, {run_time|"%Y%m%d"} its
// time formatted, optionally offset ({run_time+25h|"%Y%m%d"}), and \{ and
// \} are literal braces
// (https://docs.cloud.google.com/bigquery/docs/scheduling-queries).
func RenderTableTemplate(tmpl string, runTime time.Time) (string, error) {
	runTime = runTime.UTC()
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		switch {
		case c == '\\' && i+1 < len(tmpl) && (tmpl[i+1] == '{' || tmpl[i+1] == '}'):
			b.WriteByte(tmpl[i+1])
			i++
		case c == '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("destination_table_name_template %q has an unclosed {", tmpl)
			}
			param := tmpl[i+1 : i+end]
			out, err := renderParam(param, runTime)
			if err != nil {
				return "", fmt.Errorf("destination_table_name_template %q: %w", tmpl, err)
			}
			b.WriteString(out)
			i += end
		case c == '}':
			return "", fmt.Errorf("destination_table_name_template %q has an unmatched }", tmpl)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func renderParam(param string, runTime time.Time) (string, error) {
	if param == "run_date" {
		return runTime.Format("20060102"), nil
	}
	m := templateParamRE.FindStringSubmatch(param)
	if m == nil {
		return "", fmt.Errorf("{%s} is not a template parameter: use {run_date} or {run_time[+-offset]|\"format\"}", param)
	}
	t := runTime
	if m[1] != "" {
		v, _ := strconv.ParseFloat(m[2], 64)
		unit := map[string]float64{"h": float64(time.Hour), "m": float64(time.Minute), "s": float64(time.Second)}[m[3]]
		d := time.Duration(v * unit)
		if m[1] == "-" {
			d = -d
		}
		t = t.Add(d)
	}
	return strftime.FormatTime(m[4], &t, strftime.FormatTypeTimestamp)
}
