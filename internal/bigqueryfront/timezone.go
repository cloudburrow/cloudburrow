package bigqueryfront

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// default_time_zone (#1417, projectoptions.go). BigQuery: "the default
// time zone to use in time zone-dependent GoogleSQL functions, when a time
// zone is not specified as an argument"
// (https://cloud.google.com/bigquery/docs/default-configuration).
//
// The front sends the zone with the query, in the header
// X-Cloudburrow-Default-Time-Zone (TimeZoneHeader), and the engine
// (googlesqlite patch 0047, bigquery-emulator patch 0037, #1456) uses it
// where the resolved types show a call or cast takes a zone and is given
// none: DATE, DATETIME, TIME, STRING, EXTRACT, TIMESTAMP_TRUNC,
// DATE_TRUNC, DATETIME_TRUNC and FORMAT_TIMESTAMP of a TIMESTAMP, column
// or not; TIMESTAMP(x), PARSE_TIMESTAMP, CURRENT_DATE, CURRENT_DATETIME
// and CURRENT_TIME; a cast between TIMESTAMP and STRING, DATE, DATETIME
// or TIME; and a TIMESTAMP literal with no zone ("If no time zone is
// specified, the default time zone is used",
// https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#timestamp_literals).
// The query text is not changed, so the job keeps the client's.
//
// Still in UTC (docs/compatibility.md): a string literal the analyzer
// coerces to a TIMESTAMP, as in ts > '2020-01-01 00:00:00'. The front's
// own queries (send) run in UTC, and the SQL it writes into a client's
// query (partition decorators, partitionreads.go) names UTC.

// validTimeZone reports whether BigQuery reads s as a time zone: a tz
// database name, or an offset from UTC (+05, -08:00, UTC+3).
func validTimeZone(s string) bool {
	if offsetZone.MatchString(s) {
		return true
	}
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}

var offsetZone = regexp.MustCompile(`^(?i:UTC)?([+-])(\d{1,2})(:?(\d{2}))?$`)

// engineZone writes a zone validTimeZone takes as the engine reads one: a
// tz database name as it is, an offset as +HH:MM.
func engineZone(zone string) string {
	m := offsetZone.FindStringSubmatch(zone)
	if m == nil {
		return zone
	}
	hh, mm := m[2], m[4]
	if len(hh) == 1 {
		hh = "0" + hh
	}
	if mm == "" {
		mm = "00"
	}
	return fmt.Sprintf("%s%s:%s", m[1], hh, mm)
}

// defaultZoneKey carries a request's default time zone, as engineZone
// writes it, to TimeZoneHeader; "" is UTC.
type defaultZoneKey struct{}

// inUTC returns ctx without a default time zone.
func inUTC(ctx context.Context) context.Context {
	if z, _ := ctx.Value(defaultZoneKey{}).(string); z == "" {
		return ctx
	}
	return context.WithValue(ctx, defaultZoneKey{}, "")
}

// timeZoneHeader is the header bigquery-emulator patch 0037 reads.
const timeZoneHeader = "X-Cloudburrow-Default-Time-Zone"

// TimeZoneHeader returns next, the path to the emulator, with each
// request whose context carries a default time zone (withDefaultTimeZone)
// sent with it in the header the engine reads (#1456).
func TimeZoneHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if z, _ := r.Context().Value(defaultZoneKey{}).(string); z != "" {
			r = r.Clone(r.Context())
			r.Header.Set(timeZoneHeader, z)
		} else if r.Header.Get(timeZoneHeader) != "" {
			r = r.Clone(r.Context())
			r.Header.Del(timeZoneHeader) // a client's own is not the project's
		}
		next.ServeHTTP(w, r)
	})
}

// withDefaultTimeZone serves q with the default_time_zone of the job's
// region, when one other than UTC is set, sent to the engine (above), and
// reports whether it did.
func (f front) withDefaultTimeZone(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.settings == nil || r.Context().Value(defaultZoneKey{}) != nil || !f.settings.hasAny("default_time_zone") {
		return false
	}
	loc := queryLocation(r, insert)
	if loc == "" {
		loc = f.settings.defaultLocation(projectOf(f.base))
	}
	zone := f.settings.effective(projectOf(f.base), loc, "default_time_zone")
	if zone == "" || strings.EqualFold(zone, "UTC") || !validTimeZone(zone) {
		return false
	}
	f.runQuery(w, r.WithContext(context.WithValue(r.Context(), defaultZoneKey{}, engineZone(zone))), q, insert)
	return true
}

// hasAny reports whether any project or the organization sets option
// name, in any region.
func (p *projectOptions) hasAny(name string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, opts := range p.opts {
		for k := range opts {
			if k == name || strings.HasSuffix(k, "."+name) {
				return true
			}
		}
	}
	return false
}
