package bigqueryfront

import (
	"fmt"
	"net/http"
)

// Continuous queries (#2094).
//
// A query job's configuration.query.continuous runs it as a continuous
// query, which "must" run on "an Enterprise edition or Enterprise Plus
// edition reservation with a CONTINUOUS assignment type ... Continuous
// queries don't support the on-demand compute billing model"
// (https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction,
// Reservation limitations, read 2026-11-23). This instance serves no
// BigQuery Reservation API, so no project has such an assignment and every
// project is billed on demand: a continuous query job is refused, as
// BigQuery refuses one in a project without a CONTINUOUS assignment. The
// engine would otherwise run it once as an ordinary query, which is not
// what the client asked for. The Reservation API and a continuous query
// engine are tracked as their own work (#2094's follow-up).
//
// Google does not document the refusal's text; this one names the rule
// (UNVERIFIED (re-read 2026-11-23:
// https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction):
// the message and the 400 are not documented).
func refuseContinuous(w http.ResponseWriter, project string, job jobBody) bool {
	q := job.Configuration.Query
	if q == nil || !q.Continuous {
		return false
	}
	writeError(w, http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Continuous queries must run on a reservation with a CONTINUOUS assignment; "+
		"project %s has none and is billed on demand, which continuous queries do not support", project))
	return true
}
