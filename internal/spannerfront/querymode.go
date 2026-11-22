package spannerfront

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Query modes (#2017, #2029). Google documents ExecuteSqlRequest.query_mode
// (google/spanner/v1/spanner.proto): NORMAL returns the results; PLAN the
// query plan and no results; PROFILE the plan, the results and the
// statistics; WITH_STATS "the overall (but not operator-level) execution
// statistics along with the results"; WITH_PLAN_AND_STATS the plan, the
// overall statistics and the results.
//
// Measured on the emulator (cloud-spanner-emulator 1.5.58, 2026-11-20): PLAN
// and PROFILE answer a plan of one node, display name "No query plan" and
// no kind (its README: "The SQL query modes PLAN and PROFILE do not return
// any query plans"); PROFILE's statistics are rows_returned and
// elapsed_time; WITH_STATS and WITH_PLAN_AND_STATS return the results with
// no statistics at all. The emulator runs a query on GoogleSQL's reference
// evaluator, which has no plan.
//
// So the front serves the modes with a plan itself (plan.go, planstats.go):
// it plans the query from its text and the database's schema, in the
// operators of Google's query execution operators pages, and in PROFILE
// mode measures every operator on the engine. A query its planner does not
// understand is refused UNIMPLEMENTED, by name. WITH_STATS, which has no
// plan, runs as PROFILE on the emulator, its placeholder plan dropped.
// NORMAL passes as it is.

var restExecuteSQL = regexp.MustCompile(`^/v1/(` + pDB + `/sessions/[^/:]+):(executeSql|executeStreamingSql)$`)

// planModes are the modes whose answer includes a query plan.
var planModes = map[spannerpb.ExecuteSqlRequest_QueryMode]bool{
	spannerpb.ExecuteSqlRequest_PLAN:                true,
	spannerpb.ExecuteSqlRequest_PROFILE:             true,
	spannerpb.ExecuteSqlRequest_WITH_PLAN_AND_STATS: true,
}

// emulatorMode is a request in a mode without a plan as the emulator is
// sent it, and whether its plan is to be dropped from the answer:
// WITH_STATS runs as PROFILE.
func emulatorMode(req *spannerpb.ExecuteSqlRequest) (*spannerpb.ExecuteSqlRequest, bool) {
	if req.GetQueryMode() != spannerpb.ExecuteSqlRequest_WITH_STATS {
		return req, false
	}
	r := proto.Clone(req).(*spannerpb.ExecuteSqlRequest)
	r.QueryMode = spannerpb.ExecuteSqlRequest_PROFILE
	return r, true
}

// withoutPlan drops the emulator's placeholder plan from stats.
func withoutPlan(stats *spannerpb.ResultSetStats) {
	if stats != nil {
		stats.QueryPlan = nil
	}
}

func (d cacheData) ExecuteSql(ctx context.Context, req *spannerpb.ExecuteSqlRequest) (*spannerpb.ResultSet, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.execute(ctx, req)
}

// execute answers ExecuteSql once its caller is checked.
func (d cacheData) execute(ctx context.Context, req *spannerpb.ExecuteSqlRequest) (*spannerpb.ResultSet, error) {
	if planModes[req.GetQueryMode()] {
		var rs *spannerpb.ResultSet
		stats, err := d.s.servePlanned(ctx, req, func(r *spannerpb.ExecuteSqlRequest) (*spannerpb.ResultSetStats, []byte, error) {
			out, err := d.s.data.ExecuteSql(ctx, r)
			if err != nil {
				return nil, nil, err
			}
			rs = out
			return out.GetStats(), out.GetMetadata().GetTransaction().GetId(), nil
		})
		if err != nil {
			return nil, err
		}
		rs.Stats = stats
		return rs, nil
	}
	req, strip := emulatorMode(req)
	rs, err := d.s.data.ExecuteSql(ctx, req)
	if err == nil && strip {
		withoutPlan(rs.GetStats())
	}
	return rs, err
}

func (d cacheData) ExecuteStreamingSql(req *spannerpb.ExecuteSqlRequest, stream spannerpb.Spanner_ExecuteStreamingSqlServer) error {
	if err := d.check(stream.Context(), req.GetSession()); err != nil {
		return err
	}
	return d.executeStreaming(stream.Context(), req, stream.Send)
}

// executeStreaming answers ExecuteStreamingSql once its caller is checked.
func (d cacheData) executeStreaming(ctx context.Context, req *spannerpb.ExecuteSqlRequest, send func(*spannerpb.PartialResultSet) error) error {
	if planModes[req.GetQueryMode()] {
		var last *spannerpb.PartialResultSet
		stats, err := d.s.servePlanned(ctx, req, func(r *spannerpb.ExecuteSqlRequest) (*spannerpb.ResultSetStats, []byte, error) {
			in, err := d.s.data.ExecuteStreamingSql(ctx, r)
			if err != nil {
				return nil, nil, err
			}
			var began []byte
			for {
				m, err := in.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, nil, err
				}
				if began == nil {
					began = m.GetMetadata().GetTransaction().GetId()
				}
				if last != nil {
					if err := send(last); err != nil {
						return nil, nil, err
					}
				}
				last = m
			}
			return last.GetStats(), began, nil
		})
		if err != nil {
			return err
		}
		if last == nil {
			last = &spannerpb.PartialResultSet{}
		}
		last.Stats = stats
		return send(last)
	}
	req, strip := emulatorMode(req)
	in, err := d.s.data.ExecuteStreamingSql(ctx, req)
	if err != nil {
		return err
	}
	for {
		m, err := in.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strip {
			withoutPlan(m.GetStats())
		}
		if err := send(m); err != nil {
			return err
		}
	}
}

// restQueryMode is the query mode of a REST executeSql or
// executeStreamingSql call, read from its body, which is put back for
// whoever answers the call; ok is false for any other call.
func restQueryMode(r *http.Request) (spannerpb.ExecuteSqlRequest_QueryMode, bool) {
	if r.Method != http.MethodPost || !restExecuteSQL.MatchString(r.URL.Path) || r.Body == nil {
		return 0, false
	}
	b, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return 0, false
	}
	var body struct {
		QueryMode json.RawMessage `json:"queryMode"`
	}
	if json.Unmarshal(b, &body) != nil || len(body.QueryMode) == 0 {
		return spannerpb.ExecuteSqlRequest_NORMAL, true
	}
	var name string
	if json.Unmarshal(body.QueryMode, &name) == nil {
		return spannerpb.ExecuteSqlRequest_QueryMode(spannerpb.ExecuteSqlRequest_QueryMode_value[name]), true
	}
	var n int32
	if json.Unmarshal(body.QueryMode, &n) == nil {
		return spannerpb.ExecuteSqlRequest_QueryMode(n), true
	}
	return spannerpb.ExecuteSqlRequest_NORMAL, true
}

// claimsRESTQuery says whether the front answers a REST query call: one
// in a mode with a plan, or WITH_STATS.
func claimsRESTQuery(r *http.Request) bool {
	m, ok := restQueryMode(r)
	return ok && (planModes[m] || m == spannerpb.ExecuteSqlRequest_WITH_STATS)
}

// serveRESTQuery answers a REST query call claimsRESTQuery claimed, as the
// gRPC methods do: executeSql's ResultSet, or executeStreamingSql's
// PartialResultSets as a JSON array.
func (s *Instances) serveRESTQuery(w http.ResponseWriter, r *http.Request) {
	m := restExecuteSQL.FindStringSubmatch(r.URL.Path)
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	req := &spannerpb.ExecuteSqlRequest{}
	if err == nil {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, req)
	}
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
		return
	}
	req.Session = m[1]
	d := cacheData{s: s}
	ctx := r.Context()
	if err := d.check(ctx, req.GetSession()); err != nil {
		writeError(w, err)
		return
	}
	var out []byte
	if m[2] == "executeSql" {
		rs, err := d.execute(ctx, req)
		if err != nil {
			writeError(w, err)
			return
		}
		out, err = protojson.Marshal(rs)
		if err != nil {
			writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
			return
		}
	} else {
		parts := []json.RawMessage{}
		err := d.executeStreaming(ctx, req, func(prs *spannerpb.PartialResultSet) error {
			j, err := protojson.Marshal(prs)
			if err != nil {
				return status.Errorf(codes.Internal, "encoding the response: %v", err)
			}
			parts = append(parts, j)
			return nil
		})
		if err != nil {
			writeError(w, err)
			return
		}
		out, _ = json.Marshal(parts)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(out)
}
