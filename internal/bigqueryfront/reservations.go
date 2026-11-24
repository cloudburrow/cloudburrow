package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The BigQuery Reservation API (#2193), bigqueryreservation.googleapis.com
// v1, served on the BigQuery front's own port as the Connection API is
// (connections.go), and over gRPC on its gRPC port (reservationgrpc.go),
// as https://docs.cloud.google.com/bigquery/docs/reference/reservations/rest
// documents it (read 2026-11-23): reservations (create, get, list, patch,
// delete, failoverReservation and the IAM methods), their assignments
// (create, list, patch, delete, move), searchAssignments and
// searchAllAssignments, capacity commitments (create, get, list, patch,
// delete, split, merge), reservation groups (create, get, list, delete)
// and the BI reservation (get, update).
//
// Nothing here is capacity: an instance runs every job on the machine it
// runs on. What the API keeps decides what BigQuery decides with it: a
// continuous query runs only in a project with a CONTINUOUS assignment to
// an Enterprise or Enterprise Plus reservation (continuousjob.go), and
// INFORMATION_SCHEMA.JOBS names the reservation and edition of such a job.
//
// The documented rules enforced (reference pages and
// https://docs.cloud.google.com/bigquery/docs/reservations-tasks,
// https://docs.cloud.google.com/bigquery/docs/reservations-assignments,
// https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction,
// read 2026-11-23):
//
//   - a reservation ID "must only contain lower case alphanumeric
//     characters or dashes. It must start with a letter and must not end
//     with a dash. Its maximum length is 64 characters"; an assignment ID
//     lower case alphanumerics or dashes, at most 64 (generated when
//     empty); a capacity commitment ID the same, "The first and last
//     character cannot be a dash";
//   - "Baseline slots can only be configured in multiples of 50", and the
//     autoscaling maximum too; "You cannot set the slot_capacity option and
//     the standard edition option in the same reservation"; the edition
//     cannot change; max_slots goes with scaling_mode and is more than
//     slot_capacity; AUTOSCALE_ONLY needs ignore_idle_slots, IDLE_SLOTS_ONLY
//     and ALL_SLOTS not;
//   - delete "Returns google.rpc.Code.FAILED_PRECONDITION when reservation
//     has assignments";
//   - an assignee is projects/…, folders/… or organizations/…, and "can
//     only have one assignment per each (jobType, location) combination";
//     JOB_TYPE_UNSPECIFIED is INVALID_ARGUMENT; "Reservations with
//     continuous assignments cannot be mixed with non-continuous
//     assignments"; "Folder and organization assignments aren't available
//     to standard edition reservations"; the reservation none takes QUERY
//     assignments only;
//   - a commitment's plan is not COMMITMENT_PLAN_UNSPECIFIED or NONE, its
//     renewal plan ANNUAL, TRIAL or NONE (and THREE_YEAR's own), it ends
//     its plan's length after it starts, it cannot be deleted before then
//     (FAILED_PRECONDITION), a split leaves two of the same plan and end,
//     and a merge takes commitments of one plan.
//
// UNVERIFIED (docs re-read 2026-11-23: the pages above): the messages of
// the refusals, which Google does not give; that a commitment's slot count
// is a positive multiple of 50, as baseline slots are; a list's default
// page size (100). A reservation group's parent group and a reservation's
// scheduling policy are kept as given.

const (
	reservationPrefix = "/v1/projects/"
	maxReservationID  = 64
)

var (
	reservationIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	assignmentIDPattern  = regexp.MustCompile(`^[a-z0-9-]+$`)
	commitmentIDPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	assigneePattern      = regexp.MustCompile(`^(projects|folders|organizations)/([^/]+)$`)
)

// planDays are the commitment plans' lengths, as the CapacityCommitment
// reference gives them (FLEX: one minute).
var planDays = map[reservationpb.CapacityCommitment_CommitmentPlan]time.Duration{
	reservationpb.CapacityCommitment_FLEX:              time.Minute,
	reservationpb.CapacityCommitment_FLEX_FLAT_RATE:    time.Minute,
	reservationpb.CapacityCommitment_TRIAL:             182 * 24 * time.Hour,
	reservationpb.CapacityCommitment_MONTHLY:           30 * 24 * time.Hour,
	reservationpb.CapacityCommitment_MONTHLY_FLAT_RATE: 30 * 24 * time.Hour,
	reservationpb.CapacityCommitment_ANNUAL:            365 * 24 * time.Hour,
	reservationpb.CapacityCommitment_ANNUAL_FLAT_RATE:  365 * 24 * time.Hour,
	reservationpb.CapacityCommitment_THREE_YEAR:        1095 * 24 * time.Hour,
}

// bqReservations are every project's reservations, assignments,
// commitments, groups and BI reservations, by resource name.
type bqReservations struct {
	mu          sync.Mutex
	reservation map[string]*reservationpb.Reservation
	assignment  map[string]*reservationpb.Assignment
	commitment  map[string]*reservationpb.CapacityCommitment
	group       map[string]*reservationpb.ReservationGroup
	bi          map[string]*reservationpb.BiReservation
	policy      map[string]json.RawMessage
	etag        map[string]int64
	path        string
	logf        func(string, ...any)
	now         func() time.Time
	numbers     *projectNumbers
}

func reservationsStateFile(dir string) string { return filepath.Join(dir, "reservations.json") }

type reservationsState struct {
	Reservations []json.RawMessage          `json:"reservations,omitempty"`
	Assignments  []json.RawMessage          `json:"assignments,omitempty"`
	Commitments  []json.RawMessage          `json:"commitments,omitempty"`
	Groups       []json.RawMessage          `json:"groups,omitempty"`
	BI           []json.RawMessage          `json:"bi,omitempty"`
	Policies     map[string]json.RawMessage `json:"policies,omitempty"`
	Etags        map[string]int64           `json:"etags,omitempty"`
}

func (s *bqReservations) init() {
	if s.reservation == nil {
		s.reservation = map[string]*reservationpb.Reservation{}
		s.assignment = map[string]*reservationpb.Assignment{}
		s.commitment = map[string]*reservationpb.CapacityCommitment{}
		s.group = map[string]*reservationpb.ReservationGroup{}
		s.bi = map[string]*reservationpb.BiReservation{}
		s.policy = map[string]json.RawMessage{}
		s.etag = map[string]int64{}
	}
}

// keep restores the reservations from path, and keeps them there.
func (s *bqReservations) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var saved reservationsState
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the reservations it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the reservations it kept", path)
		saved = reservationsState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservation = nil
	s.init()
	u := protojson.UnmarshalOptions{DiscardUnknown: true}
	for _, raw := range saved.Reservations {
		m := &reservationpb.Reservation{}
		if u.Unmarshal(raw, m) == nil {
			s.reservation[m.GetName()] = m
		}
	}
	for _, raw := range saved.Assignments {
		m := &reservationpb.Assignment{}
		if u.Unmarshal(raw, m) == nil {
			s.assignment[m.GetName()] = m
		}
	}
	for _, raw := range saved.Commitments {
		m := &reservationpb.CapacityCommitment{}
		if u.Unmarshal(raw, m) == nil {
			s.commitment[m.GetName()] = m
		}
	}
	for _, raw := range saved.Groups {
		m := &reservationpb.ReservationGroup{}
		if u.Unmarshal(raw, m) == nil {
			s.group[m.GetName()] = m
		}
	}
	for _, raw := range saved.BI {
		m := &reservationpb.BiReservation{}
		if u.Unmarshal(raw, m) == nil {
			s.bi[m.GetName()] = m
		}
	}
	for k, v := range saved.Policies {
		s.policy[k] = v
	}
	for k, v := range saved.Etags {
		s.etag[k] = v
	}
	s.path, s.logf = path, logf
}

func (s *bqReservations) saveLocked() {
	if s.path == "" {
		return
	}
	st := reservationsState{Policies: s.policy, Etags: s.etag}
	add := func(out *[]json.RawMessage, names []string, get func(string) proto.Message) {
		sort.Strings(names)
		for _, n := range names {
			if b, err := protojson.Marshal(get(n)); err == nil {
				*out = append(*out, b)
			}
		}
	}
	add(&st.Reservations, resvKeys(s.reservation), func(n string) proto.Message { return s.reservation[n] })
	add(&st.Assignments, resvKeys(s.assignment), func(n string) proto.Message { return s.assignment[n] })
	add(&st.Commitments, resvKeys(s.commitment), func(n string) proto.Message { return s.commitment[n] })
	add(&st.Groups, resvKeys(s.group), func(n string) proto.Message { return s.group[n] })
	add(&st.BI, resvKeys(s.bi), func(n string) proto.Message { return s.bi[n] })
	b, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(s.path, b)
	}
	if err != nil && s.logf != nil {
		s.logf("bigquery front: keep the reservations: %v", err)
	}
}

func resvKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *bqReservations) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// reservationLocation is a location as BigQuery names it: the
// multi-regions upper case (US, EU), a region lower case.
func reservationLocation(l string) string {
	if u := strings.ToUpper(l); u == "US" || u == "EU" {
		return u
	}
	return strings.ToLower(l)
}

// projectID is a path's project as its ID: a number the host sent is its
// project's ID.
func (s *bqReservations) projectID(p string) string {
	if n, err := strconv.ParseInt(p, 10, 64); err == nil && n > 0 {
		if id, ok := s.numbers.idOf(n); ok {
			return id
		}
	}
	return p
}

// assigneeKey is an assignee as the assignments are compared: a project
// by its ID.
func (s *bqReservations) assigneeKey(a string) string {
	m := assigneePattern.FindStringSubmatch(a)
	if m == nil {
		return a
	}
	if m[1] == "projects" {
		return "projects/" + s.projectID(m[2])
	}
	return a
}

// googleError answers with the standard Google API error, whose status
// the gRPC transport maps to its code (reservationgrpc.go).
func googleError(w http.ResponseWriter, code int, status, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}})
}

type resvErr struct {
	code   int
	status string
	msg    string
}

func (e *resvErr) Error() string { return e.msg }

func invalidArg(format string, a ...any) *resvErr {
	return &resvErr{400, "INVALID_ARGUMENT", fmt.Sprintf(format, a...)}
}
func failedPrecondition(format string, a ...any) *resvErr {
	return &resvErr{400, "FAILED_PRECONDITION", fmt.Sprintf(format, a...)}
}
func notFoundErr(format string, a ...any) *resvErr {
	return &resvErr{404, "NOT_FOUND", fmt.Sprintf(format, a...)}
}
func alreadyExists(format string, a ...any) *resvErr {
	return &resvErr{409, "ALREADY_EXISTS", fmt.Sprintf(format, a...)}
}

// resvRequest is one call of the API, parsed from its path.
type resvRequest struct {
	method                   string
	project, location        string // canonical
	parent                   string // projects/P/locations/L
	collection               string // reservations, capacityCommitments, reservationGroups, biReservation, assignments
	id                       string // the reservation, commitment or group
	assignment               string // under reservations/id/assignments
	verb                     string // after ':'
	query                    map[string][]string
	body                     []byte
	onCollection, onLocation bool
}

// serve answers the Reservation API's paths; it reports whether path was
// one.
func (s *bqReservations) serve(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, reservationPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(p, "/v1/"), "/")
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "locations" {
		return false
	}
	req := resvRequest{method: r.Method, query: r.URL.Query()}
	loc := parts[3]
	if i := strings.IndexByte(loc, ':'); i >= 0 {
		loc, req.verb = loc[:i], loc[i+1:]
	}
	rest := parts[4:]
	switch {
	case len(rest) == 0:
		if req.verb != "searchAssignments" && req.verb != "searchAllAssignments" && req.verb != "upgradeEdition" {
			return false
		}
		req.onLocation = true
	default:
		coll := rest[0]
		if i := strings.IndexByte(coll, ':'); i >= 0 {
			if len(rest) != 1 {
				return false
			}
			coll, req.verb = coll[:i], coll[i+1:]
			req.onCollection = true
		}
		switch coll {
		case "reservations", "capacityCommitments", "reservationGroups", "biReservation":
		default:
			return false
		}
		req.collection = coll
		switch len(rest) {
		case 1:
			req.onCollection = req.onCollection || coll != "biReservation"
		case 2:
			req.id = rest[1]
		case 3, 4:
			if coll != "reservations" || rest[2] != "assignments" {
				return false
			}
			req.id = rest[1]
			req.collection = "assignments"
			if len(rest) == 3 {
				req.onCollection = true
			} else {
				req.assignment = rest[3]
			}
		default:
			return false
		}
		last := &req.id
		if req.assignment != "" {
			last = &req.assignment
		}
		if i := strings.IndexByte(*last, ':'); i >= 0 {
			*last, req.verb = (*last)[:i], (*last)[i+1:]
		}
		if req.collection == "assignments" && req.onCollection {
			if i := strings.IndexByte(req.id, ':'); i >= 0 {
				return false
			}
		}
	}
	req.project, req.location = s.projectID(parts[1]), reservationLocation(loc)
	req.parent = "projects/" + req.project + "/locations/" + req.location
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			googleError(w, 400, "INVALID_ARGUMENT", "Invalid request body: "+err.Error())
			return true
		}
		req.body = b
	}
	s.mu.Lock()
	s.init()
	out, err := s.call(req)
	if err == nil && req.method != http.MethodGet {
		s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		var re *resvErr
		if errors.As(err, &re) {
			googleError(w, re.code, re.status, re.msg)
		} else {
			googleError(w, 500, "INTERNAL", err.Error())
		}
		return true
	}
	if m, ok := out.(proto.Message); ok {
		b, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(m)
		if err != nil {
			googleError(w, 500, "INTERNAL", err.Error())
			return true
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(200)
		_, _ = w.Write(b)
		return true
	}
	writeJSON(w, 200, out)
	return true
}

func methodNotAllowed(req resvRequest) error {
	return &resvErr{405, "INVALID_ARGUMENT", "Method " + req.method + " is not allowed here"}
}

// decodeInto reads a request body into m, as Google's JSON transcoding
// does: an unknown field is INVALID_ARGUMENT.
func decodeInto(b []byte, m proto.Message) error {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return invalidArg("Invalid JSON payload received. %v", err)
	}
	return nil
}

// bodyKeys are the top-level fields a JSON body names.
func bodyKeys(b []byte) []string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return resvKeys(m)
}

func (s *bqReservations) call(req resvRequest) (any, error) {
	if req.onLocation {
		if req.verb == "upgradeEdition" {
			if req.method != http.MethodPost {
				return nil, methodNotAllowed(req)
			}
			return s.upgradeEdition(req)
		}
		if req.method != http.MethodGet {
			return nil, methodNotAllowed(req)
		}
		return s.search(req)
	}
	switch req.collection {
	case "reservations":
		return s.callReservation(req)
	case "assignments":
		return s.callAssignment(req)
	case "capacityCommitments":
		return s.callCommitment(req)
	case "reservationGroups":
		return s.callGroup(req)
	case "biReservation":
		return s.callBI(req)
	}
	return nil, notFoundErr("Not found")
}

// page cuts names (sorted) to one page of a list: pageSize (default
// 100) and pageToken, the name to start at.
func page(names []string, q map[string][]string) ([]string, string, error) {
	sort.Strings(names)
	size := 100
	if v := first(q["pageSize"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, "", invalidArg("Invalid page_size %q", v)
		}
		if n > 0 {
			size = n
		}
	}
	start := 0
	if tok := first(q["pageToken"]); tok != "" {
		start = sort.SearchStrings(names, tok)
	}
	names = names[start:]
	if len(names) > size {
		return names[:size], names[size], nil
	}
	return names, "", nil
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// checkSlots checks a reservation's slots and scaling, as the reference
// and reservations-tasks give them.
func checkSlots(rv *reservationpb.Reservation) error {
	if rv.GetSlotCapacity() < 0 {
		return invalidArg("slot_capacity must not be negative")
	}
	if rv.GetSlotCapacity()%50 != 0 {
		return invalidArg("Baseline slots can only be configured in multiples of 50; slot_capacity is %d", rv.GetSlotCapacity())
	}
	if as := rv.GetAutoscale(); as != nil {
		if as.GetMaxSlots() < 0 || as.GetMaxSlots()%50 != 0 {
			return invalidArg("autoscale.max_slots must be a multiple of 50; it is %d", as.GetMaxSlots())
		}
	}
	if rv.GetEdition() == reservationpb.Edition_STANDARD && rv.GetSlotCapacity() > 0 {
		return invalidArg("You cannot set the slot_capacity option and the standard edition option in the same reservation")
	}
	hasMax := rv.MaxSlots != nil && rv.GetMaxSlots() != 0
	hasMode := rv.GetScalingMode() != reservationpb.Reservation_SCALING_MODE_UNSPECIFIED
	if hasMax != hasMode {
		return invalidArg("max_slots and scaling_mode must be set together")
	}
	if hasMax {
		if rv.GetMaxSlots() <= rv.GetSlotCapacity() {
			return invalidArg("max_slots (%d) must be greater than slot_capacity (%d)", rv.GetMaxSlots(), rv.GetSlotCapacity())
		}
		if rv.GetMaxSlots()%50 != 0 {
			return invalidArg("max_slots must be a multiple of 50; it is %d", rv.GetMaxSlots())
		}
		if rv.GetAutoscale().GetMaxSlots() != 0 {
			return invalidArg("autoscale.max_slots cannot be set with max_slots and scaling_mode")
		}
		switch rv.GetScalingMode() {
		case reservationpb.Reservation_AUTOSCALE_ONLY:
			if !rv.GetIgnoreIdleSlots() {
				return invalidArg("scaling_mode AUTOSCALE_ONLY requires ignore_idle_slots to be true")
			}
		case reservationpb.Reservation_IDLE_SLOTS_ONLY, reservationpb.Reservation_ALL_SLOTS:
			if rv.GetIgnoreIdleSlots() {
				return invalidArg("scaling_mode %s requires ignore_idle_slots to be false", rv.GetScalingMode())
			}
		}
	}
	if rv.GetConcurrency() < 0 {
		return invalidArg("concurrency must not be negative")
	}
	return nil
}

func (s *bqReservations) reservationName(req resvRequest) string {
	return req.parent + "/reservations/" + req.id
}

func (s *bqReservations) hasAssignments(name string) bool {
	for n := range s.assignment {
		if strings.HasPrefix(n, name+"/assignments/") {
			return true
		}
	}
	return false
}

func (s *bqReservations) callReservation(req resvRequest) (any, error) {
	now := timestamppb.New(s.clock())
	if req.onCollection {
		switch req.method {
		case http.MethodPost:
			id := first(req.query["reservationId"])
			if id == "" {
				return nil, invalidArg("reservation_id is required")
			}
			if len(id) > maxReservationID || !reservationIDPattern.MatchString(id) {
				return nil, invalidArg("Invalid reservation_id %q: it must only contain lower case alphanumeric characters or dashes. "+
					"It must start with a letter and must not end with a dash. Its maximum length is 64 characters", id)
			}
			name := req.parent + "/reservations/" + id
			if _, ok := s.reservation[name]; ok || id == "none" {
				return nil, alreadyExists("Reservation %s already exists", name)
			}
			rv := &reservationpb.Reservation{}
			if err := decodeInto(req.body, rv); err != nil {
				return nil, err
			}
			if err := checkSecondary(rv.GetEdition(), req.location, rv.GetSecondaryLocation()); err != nil {
				return nil, err
			}
			if err := checkSlots(rv); err != nil {
				return nil, err
			}
			if g := rv.GetReservationGroup(); g != "" {
				if _, ok := s.group[g]; !ok {
					return nil, notFoundErr("Reservation group %s not found", g)
				}
			}
			rv.Name, rv.CreationTime, rv.UpdateTime = name, now, now
			rv.PrimaryLocation, rv.OriginalPrimaryLocation = req.location, req.location
			if rv.Autoscale != nil {
				rv.Autoscale.CurrentSlots = 0
			}
			s.reservation[name] = rv
			return rv, nil
		case http.MethodGet:
			var names []string
			for n := range s.reservation {
				if strings.HasPrefix(n, req.parent+"/reservations/") {
					names = append(names, n)
				}
			}
			pg, next, err := page(names, req.query)
			if err != nil {
				return nil, err
			}
			out := &reservationpb.ListReservationsResponse{NextPageToken: next}
			for _, n := range pg {
				out.Reservations = append(out.Reservations, s.reservation[n])
			}
			return out, nil
		}
		return nil, methodNotAllowed(req)
	}
	name := s.reservationName(req)
	rv, ok := s.reservation[name]
	if !ok {
		return nil, notFoundErr("Reservation %s not found", name)
	}
	switch req.verb {
	case "":
	case "failoverReservation":
		if req.method != http.MethodPost {
			return nil, methodNotAllowed(req)
		}
		if rv.GetSecondaryLocation() == "" {
			return nil, failedPrecondition("Reservation %s has no secondary location to fail over to", name)
		}
		rv.PrimaryLocation, rv.SecondaryLocation = rv.GetSecondaryLocation(), rv.GetPrimaryLocation()
		rv.UpdateTime = now
		return rv, nil
	case "getIamPolicy", "setIamPolicy", "testIamPermissions":
		if err := iamMethod(req); err != nil {
			return nil, err
		}
		return s.iam(name, req)
	default:
		return nil, notFoundErr("Unknown method %s", req.verb)
	}
	switch req.method {
	case http.MethodGet:
		return rv, nil
	case http.MethodDelete:
		if s.hasAssignments(name) {
			return nil, failedPrecondition("Reservation %s has assignments: delete them first", name)
		}
		delete(s.reservation, name)
		delete(s.policy, name)
		delete(s.etag, name)
		return map[string]any{}, nil
	case http.MethodPatch:
		in := &reservationpb.Reservation{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		paths := splitMask(first(req.query["updateMask"]))
		if len(paths) == 0 {
			paths = bodyKeys(req.body)
		}
		next := proto.Clone(rv).(*reservationpb.Reservation)
		for _, p := range paths {
			switch p {
			case "slot_capacity", "slotCapacity":
				next.SlotCapacity = in.GetSlotCapacity()
			case "ignore_idle_slots", "ignoreIdleSlots":
				next.IgnoreIdleSlots = in.GetIgnoreIdleSlots()
			case "autoscale", "autoscale.max_slots", "autoscale.maxSlots":
				if in.Autoscale == nil {
					next.Autoscale = nil
				} else {
					next.Autoscale = &reservationpb.Reservation_Autoscale{MaxSlots: in.GetAutoscale().GetMaxSlots(), CurrentSlots: rv.GetAutoscale().GetCurrentSlots()}
				}
			case "concurrency":
				next.Concurrency = in.GetConcurrency()
			case "edition":
				if in.GetEdition() != rv.GetEdition() {
					return nil, invalidArg("The edition of a reservation cannot be changed: delete the reservation and create it again")
				}
			case "secondary_location", "secondaryLocation":
				if err := checkSecondary(rv.GetEdition(), req.location, in.GetSecondaryLocation()); err != nil {
					return nil, err
				}
				next.SecondaryLocation = in.GetSecondaryLocation()
			case "max_slots", "maxSlots":
				next.MaxSlots = in.MaxSlots
			case "scaling_mode", "scalingMode":
				next.ScalingMode = in.GetScalingMode()
			case "labels":
				next.Labels = in.GetLabels()
			case "reservation_group", "reservationGroup":
				if g := in.GetReservationGroup(); g != "" {
					if _, ok := s.group[g]; !ok {
						return nil, notFoundErr("Reservation group %s not found", g)
					}
				}
				next.ReservationGroup = in.GetReservationGroup()
			case "scheduling_policy", "schedulingPolicy":
				next.SchedulingPolicy = in.GetSchedulingPolicy()
			case "multi_region_auxiliary", "multiRegionAuxiliary":
				next.MultiRegionAuxiliary = in.GetMultiRegionAuxiliary()
			case "name", "creation_time", "creationTime", "update_time", "updateTime", "primary_location", "primaryLocation",
				"original_primary_location", "originalPrimaryLocation", "replication_status", "replicationStatus":
				// Output only.
			default:
				return nil, invalidArg("Invalid update_mask path %q", p)
			}
		}
		if err := checkSlots(next); err != nil {
			return nil, err
		}
		if err := s.checkContinuousCap(name, next); err != nil {
			return nil, err
		}
		next.UpdateTime = now
		s.reservation[name] = next
		return next, nil
	}
	return nil, methodNotAllowed(req)
}

// maxContinuousSlots is the documented cap of a CONTINUOUS assignment's
// reservation: "A CONTINUOUS assignment is capped at 500 slots"
// (continuous-queries-introduction, Reservation limitations).
const maxContinuousSlots = 500

// checkContinuousCap refuses a reservation with a CONTINUOUS assignment
// whose baseline and autoscaling slots are more than the cap.
func (s *bqReservations) checkContinuousCap(name string, rv *reservationpb.Reservation) error {
	if !s.hasJobType(name, reservationpb.Assignment_CONTINUOUS) {
		return nil
	}
	if n := reservationSlots(rv); n > maxContinuousSlots {
		return invalidArg("Reservation %s has a CONTINUOUS assignment, which is capped at %d slots; it would have %d", name, maxContinuousSlots, n)
	}
	return nil
}

// reservationSlots are a reservation's most slots: its baseline and its
// autoscaling, or max_slots.
func reservationSlots(rv *reservationpb.Reservation) int64 {
	if rv.MaxSlots != nil && rv.GetMaxSlots() > 0 {
		return rv.GetMaxSlots()
	}
	return rv.GetSlotCapacity() + rv.GetAutoscale().GetMaxSlots()
}

func (s *bqReservations) hasJobType(name string, t reservationpb.Assignment_JobType) bool {
	for n, a := range s.assignment {
		if strings.HasPrefix(n, name+"/assignments/") && a.GetJobType() == t {
			return true
		}
	}
	return false
}

func splitMask(m string) []string {
	var out []string
	for _, p := range strings.Split(m, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkAssignment checks a new assignment a, under the reservation
// named res (nil for none), not counting the assignment named self.
func (s *bqReservations) checkAssignment(resName string, rv *reservationpb.Reservation, a *reservationpb.Assignment, location, self string) error {
	if a.GetJobType() == reservationpb.Assignment_JOB_TYPE_UNSPECIFIED {
		return invalidArg("job_type is required: JOB_TYPE_UNSPECIFIED is not a job type")
	}
	m := assigneePattern.FindStringSubmatch(a.GetAssignee())
	if m == nil {
		return invalidArg("Invalid assignee %q: it is projects/{project}, folders/{folder} or organizations/{organization}", a.GetAssignee())
	}
	if p := a.GetPrincipal(); p != "" && !strings.HasPrefix(p, "principal://") {
		return invalidArg("Invalid principal %q", p)
	}
	if rv == nil {
		if a.GetJobType() != reservationpb.Assignment_QUERY {
			return invalidArg("Assignments to none are supported for QUERY jobs only")
		}
	} else {
		if rv.GetEdition() == reservationpb.Edition_STANDARD && m[1] != "projects" {
			return invalidArg("Folder and organization assignments aren't available to standard edition reservations")
		}
		continuous := a.GetJobType() == reservationpb.Assignment_CONTINUOUS
		for n, other := range s.assignment {
			if n == self || !strings.HasPrefix(n, resName+"/assignments/") {
				continue
			}
			if (other.GetJobType() == reservationpb.Assignment_CONTINUOUS) != continuous {
				return invalidArg("Reservations with continuous assignments cannot be mixed with non-continuous assignments: %s", resName)
			}
		}
		if continuous {
			if n := reservationSlots(rv); n > maxContinuousSlots {
				return invalidArg("A CONTINUOUS assignment is capped at %d slots; reservation %s has %d", maxContinuousSlots, resName, n)
			}
		}
	}
	key := s.assigneeKey(a.GetAssignee())
	for n, other := range s.assignment {
		if n == self || other.GetJobType() != a.GetJobType() || s.assigneeKey(other.GetAssignee()) != key || other.GetPrincipal() != a.GetPrincipal() {
			continue
		}
		if !strings.Contains(n, "/locations/"+location+"/") {
			continue
		}
		return alreadyExists("%s already has a %s assignment in %s: %s", a.GetAssignee(), a.GetJobType(), location, n)
	}
	return nil
}

func newAssignmentID() string {
	// Google's are long decimal numbers.
	return strconv.FormatUint(rand.Uint64()>>1, 10)
}

func (s *bqReservations) callAssignment(req resvRequest) (any, error) {
	resName := s.reservationName(req)
	rv, ok := s.reservation[resName]
	if !ok && req.id != "none" {
		return nil, notFoundErr("Reservation %s not found", resName)
	}
	if req.onCollection {
		switch req.method {
		case http.MethodPost:
			a := &reservationpb.Assignment{}
			if err := decodeInto(req.body, a); err != nil {
				return nil, err
			}
			id := first(req.query["assignmentId"])
			if id == "" {
				id = newAssignmentID()
			} else if len(id) > maxReservationID || !assignmentIDPattern.MatchString(id) {
				return nil, invalidArg("Invalid assignment_id %q: it must only contain lower case alphanumeric characters or dashes. Max length is 64 characters", id)
			}
			name := resName + "/assignments/" + id
			if _, ok := s.assignment[name]; ok {
				return nil, alreadyExists("Assignment %s already exists", name)
			}
			if err := s.checkAssignment(resName, rv, a, req.location, ""); err != nil {
				return nil, err
			}
			a.Name, a.State = name, reservationpb.Assignment_ACTIVE
			s.assignment[name] = a
			return a, nil
		case http.MethodGet:
			var names []string
			for n := range s.assignment {
				if strings.HasPrefix(n, resName+"/assignments/") || req.id == "-" && strings.HasPrefix(n, req.parent+"/reservations/") {
					names = append(names, n)
				}
			}
			pg, next, err := page(names, req.query)
			if err != nil {
				return nil, err
			}
			out := &reservationpb.ListAssignmentsResponse{NextPageToken: next}
			for _, n := range pg {
				out.Assignments = append(out.Assignments, s.assignment[n])
			}
			return out, nil
		}
		return nil, methodNotAllowed(req)
	}
	name := resName + "/assignments/" + req.assignment
	a, ok := s.assignment[name]
	if !ok {
		return nil, notFoundErr("Assignment %s not found", name)
	}
	switch req.verb {
	case "":
	case "getIamPolicy", "setIamPolicy", "testIamPermissions":
		if err := iamMethod(req); err != nil {
			return nil, err
		}
		return s.iam(name, req)
	case "move":
		if req.method != http.MethodPost {
			return nil, methodNotAllowed(req)
		}
		in := &reservationpb.MoveAssignmentRequest{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		dest := in.GetDestinationId()
		var destRes *reservationpb.Reservation
		if !strings.HasSuffix(dest, "/reservations/none") {
			d := s.canonicalName(dest)
			var ok bool
			if destRes, ok = s.reservation[d]; !ok {
				return nil, notFoundErr("Reservation %s not found", dest)
			}
			dest = d
		}
		if !strings.HasPrefix(dest, req.parent+"/reservations/") {
			return nil, invalidArg("The destination reservation %s is not in %s", dest, req.parent)
		}
		id := in.GetAssignmentId()
		if id == "" {
			id = req.assignment
		} else if len(id) > maxReservationID || !assignmentIDPattern.MatchString(id) {
			return nil, invalidArg("Invalid assignment_id %q", id)
		}
		moved := proto.Clone(a).(*reservationpb.Assignment)
		if err := s.checkAssignment(dest, destRes, moved, req.location, name); err != nil {
			return nil, err
		}
		moved.Name = dest + "/assignments/" + id
		if _, ok := s.assignment[moved.Name]; ok && moved.Name != name {
			return nil, alreadyExists("Assignment %s already exists", moved.Name)
		}
		delete(s.assignment, name)
		s.assignment[moved.Name] = moved
		return moved, nil
	default:
		return nil, notFoundErr("Unknown method %s", req.verb)
	}
	switch req.method {
	case http.MethodGet:
		// The API has no assignments.get; the gRPC and REST clients
		// never send it. Answer it as Google's front would.
		return nil, &resvErr{404, "NOT_FOUND", "Method not found"}
	case http.MethodDelete:
		delete(s.assignment, name)
		return map[string]any{}, nil
	case http.MethodPatch:
		in := &reservationpb.Assignment{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		paths := splitMask(first(req.query["updateMask"]))
		if len(paths) == 0 {
			paths = bodyKeys(req.body)
		}
		next := proto.Clone(a).(*reservationpb.Assignment)
		for _, p := range paths {
			switch p {
			case "enable_gemini_in_bigquery", "enableGeminiInBigquery":
				next.EnableGeminiInBigquery = in.GetEnableGeminiInBigquery()
			case "scheduling_policy", "schedulingPolicy":
				next.SchedulingPolicy = in.GetSchedulingPolicy()
			case "name", "state":
			default:
				return nil, invalidArg("Invalid update_mask path %q: only enable_gemini_in_bigquery and scheduling_policy can be updated", p)
			}
		}
		s.assignment[name] = next
		return next, nil
	}
	return nil, methodNotAllowed(req)
}

// canonicalName is a reservation's name with its project by ID and its
// location as BigQuery names it.
func (s *bqReservations) canonicalName(n string) string {
	parts := strings.Split(n, "/")
	if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "locations" {
		parts[1], parts[3] = s.projectID(parts[1]), reservationLocation(parts[3])
	}
	return strings.Join(parts, "/")
}

// search answers searchAssignments and searchAllAssignments: query is
// assignee=projects/… (or folders/…, organizations/…), and the answer
// the assignments of that assignee in the location (with the ones of its
// ancestors, which an instance's projects have none of).
func (s *bqReservations) search(req resvRequest) (any, error) {
	q := strings.TrimSpace(first(req.query["query"]))
	var want string
	if q != "" {
		k, v, ok := strings.Cut(q, "=")
		if !ok || strings.TrimSpace(k) != "assignee" {
			return nil, invalidArg("Invalid query %q: it is assignee=projects/{project}, folders/{folder} or organizations/{organization}", q)
		}
		want = s.assigneeKey(strings.Trim(strings.TrimSpace(v), `"`))
	}
	var names []string
	for n, a := range s.assignment {
		if !strings.HasPrefix(n, req.parent+"/reservations/") {
			continue
		}
		if want != "" && s.assigneeKey(a.GetAssignee()) != want {
			continue
		}
		names = append(names, n)
	}
	pg, next, err := page(names, req.query)
	if err != nil {
		return nil, err
	}
	var list []*reservationpb.Assignment
	for _, n := range pg {
		list = append(list, s.assignment[n])
	}
	if req.verb == "searchAllAssignments" {
		return &reservationpb.SearchAllAssignmentsResponse{Assignments: list, NextPageToken: next}, nil
	}
	return &reservationpb.SearchAssignmentsResponse{Assignments: list, NextPageToken: next}, nil
}

func (s *bqReservations) callCommitment(req resvRequest) (any, error) {
	now := s.clock()
	activate := func(c *reservationpb.CapacityCommitment, start time.Time) {
		c.State = reservationpb.CapacityCommitment_ACTIVE
		c.CommitmentStartTime = timestamppb.New(start)
		c.CommitmentEndTime = timestamppb.New(start.Add(planDays[c.GetPlan()]))
		c.IsFlatRate = strings.HasSuffix(c.GetPlan().String(), "_FLAT_RATE")
	}
	if req.onCollection && req.verb == "merge" {
		if req.method != http.MethodPost {
			return nil, methodNotAllowed(req)
		}
		in := &reservationpb.MergeCapacityCommitmentsRequest{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		if len(in.GetCapacityCommitmentIds()) < 2 {
			return nil, invalidArg("Merging takes at least two capacity commitments")
		}
		var plan reservationpb.CapacityCommitment_CommitmentPlan
		var total int64
		var end time.Time
		var merged []string
		for i, id := range in.GetCapacityCommitmentIds() {
			n := req.parent + "/capacityCommitments/" + id
			c, ok := s.commitment[n]
			if !ok {
				return nil, notFoundErr("Capacity commitment %s not found", n)
			}
			if i > 0 && c.GetPlan() != plan {
				return nil, invalidArg("Capacity commitments can be merged only when they have the same plan")
			}
			plan = c.GetPlan()
			total += c.GetSlotCount()
			if e := c.GetCommitmentEndTime().AsTime(); e.After(end) {
				end = e
			}
			merged = append(merged, n)
		}
		id := in.GetCapacityCommitmentId()
		if id == "" {
			id = newAssignmentID()
		} else if len(id) > maxReservationID || !commitmentIDPattern.MatchString(id) {
			return nil, invalidArg("Invalid capacity_commitment_id %q", id)
		}
		first := s.commitment[merged[0]]
		out := &reservationpb.CapacityCommitment{Name: req.parent + "/capacityCommitments/" + id, SlotCount: total, Plan: plan,
			RenewalPlan: first.GetRenewalPlan(), Edition: first.GetEdition()}
		activate(out, now)
		out.CommitmentEndTime = timestamppb.New(end)
		for _, n := range merged {
			delete(s.commitment, n)
		}
		s.commitment[out.Name] = out
		return out, nil
	}
	if req.onCollection {
		switch req.method {
		case http.MethodPost:
			c := &reservationpb.CapacityCommitment{}
			if err := decodeInto(req.body, c); err != nil {
				return nil, err
			}
			switch c.GetPlan() {
			case reservationpb.CapacityCommitment_COMMITMENT_PLAN_UNSPECIFIED, reservationpb.CapacityCommitment_NONE:
				return nil, invalidArg("Invalid plan %s: a capacity commitment needs a commitment plan", c.GetPlan())
			}
			if err := checkRenewal(c); err != nil {
				return nil, err
			}
			if c.GetSlotCount() <= 0 || c.GetSlotCount()%50 != 0 {
				return nil, invalidArg("slot_count must be a positive multiple of 50; it is %d", c.GetSlotCount())
			}
			id := first(req.query["capacityCommitmentId"])
			if id == "" {
				id = newAssignmentID()
			} else if len(id) > maxReservationID || !commitmentIDPattern.MatchString(id) {
				return nil, invalidArg("Invalid capacity_commitment_id %q: it must only contain lower case alphanumeric characters or dashes. "+
					"The first and last character cannot be a dash. Max length is 64 characters", id)
			}
			name := req.parent + "/capacityCommitments/" + id
			if _, ok := s.commitment[name]; ok {
				return nil, alreadyExists("Capacity commitment %s already exists", name)
			}
			c.Name, c.FailureStatus = name, nil
			activate(c, now)
			s.commitment[name] = c
			return c, nil
		case http.MethodGet:
			var names []string
			for n := range s.commitment {
				if strings.HasPrefix(n, req.parent+"/capacityCommitments/") {
					names = append(names, n)
				}
			}
			pg, next, err := page(names, req.query)
			if err != nil {
				return nil, err
			}
			out := &reservationpb.ListCapacityCommitmentsResponse{NextPageToken: next}
			for _, n := range pg {
				out.CapacityCommitments = append(out.CapacityCommitments, s.commitment[n])
			}
			return out, nil
		}
		return nil, methodNotAllowed(req)
	}
	name := req.parent + "/capacityCommitments/" + req.id
	c, ok := s.commitment[name]
	if !ok {
		return nil, notFoundErr("Capacity commitment %s not found", name)
	}
	switch req.verb {
	case "":
	case "split":
		if req.method != http.MethodPost {
			return nil, methodNotAllowed(req)
		}
		in := &reservationpb.SplitCapacityCommitmentRequest{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		n := in.GetSlotCount()
		if n <= 0 || n >= c.GetSlotCount() || n%50 != 0 {
			return nil, invalidArg("slot_count must be a multiple of 50 between 0 and the commitment's %d slots", c.GetSlotCount())
		}
		first := proto.Clone(c).(*reservationpb.CapacityCommitment)
		first.SlotCount = c.GetSlotCount() - n
		second := proto.Clone(c).(*reservationpb.CapacityCommitment)
		second.SlotCount = n
		second.Name = req.parent + "/capacityCommitments/" + newAssignmentID()
		s.commitment[name] = first
		s.commitment[second.Name] = second
		return &reservationpb.SplitCapacityCommitmentResponse{First: first, Second: second}, nil
	default:
		return nil, notFoundErr("Unknown method %s", req.verb)
	}
	switch req.method {
	case http.MethodGet:
		return c, nil
	case http.MethodDelete:
		if end := c.GetCommitmentEndTime(); end != nil && now.Before(end.AsTime()) {
			return nil, failedPrecondition("Capacity commitment %s cannot be deleted before its commitment end time %s", name, end.AsTime().UTC().Format(time.RFC3339))
		}
		delete(s.commitment, name)
		return map[string]any{}, nil
	case http.MethodPatch:
		in := &reservationpb.CapacityCommitment{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		paths := splitMask(first(req.query["updateMask"]))
		if len(paths) == 0 {
			paths = bodyKeys(req.body)
		}
		next := proto.Clone(c).(*reservationpb.CapacityCommitment)
		for _, p := range paths {
			switch p {
			case "plan":
				// "Plan can only be changed to a plan of a longer commitment
				// period" (CapacityCommitment reference, update).
				if planDays[in.GetPlan()] < planDays[c.GetPlan()] || in.GetPlan() == reservationpb.CapacityCommitment_NONE {
					return nil, invalidArg("A commitment's plan can only be changed to a plan of a longer commitment period")
				}
				next.Plan = in.GetPlan()
				next.CommitmentEndTime = timestamppb.New(c.GetCommitmentStartTime().AsTime().Add(planDays[next.GetPlan()]))
			case "renewal_plan", "renewalPlan":
				next.RenewalPlan = in.GetRenewalPlan()
				if err := checkRenewal(next); err != nil {
					return nil, err
				}
			default:
				return nil, invalidArg("Invalid update_mask path %q: only plan and renewal_plan can be updated", p)
			}
		}
		s.commitment[name] = next
		return next, nil
	}
	return nil, methodNotAllowed(req)
}

func checkRenewal(c *reservationpb.CapacityCommitment) error {
	switch c.GetRenewalPlan() {
	case reservationpb.CapacityCommitment_COMMITMENT_PLAN_UNSPECIFIED, reservationpb.CapacityCommitment_NONE,
		reservationpb.CapacityCommitment_ANNUAL, reservationpb.CapacityCommitment_TRIAL:
		return nil
	case reservationpb.CapacityCommitment_THREE_YEAR:
		if c.GetPlan() == reservationpb.CapacityCommitment_THREE_YEAR {
			return nil
		}
	}
	return invalidArg("Invalid renewal_plan %s: it is ANNUAL, TRIAL or NONE", c.GetRenewalPlan())
}

func (s *bqReservations) callGroup(req resvRequest) (any, error) {
	now := timestamppb.New(s.clock())
	if req.onCollection {
		switch req.method {
		case http.MethodPost:
			id := first(req.query["reservationGroupId"])
			if id == "" || len(id) > maxReservationID || !reservationIDPattern.MatchString(id) {
				return nil, invalidArg("Invalid reservation_group_id %q: it must only contain lower case alphanumeric characters or dashes. "+
					"It must start with a letter and must not end with a dash. Its maximum length is 64 characters", id)
			}
			g := &reservationpb.ReservationGroup{}
			if err := decodeInto(req.body, g); err != nil {
				return nil, err
			}
			name := req.parent + "/reservationGroups/" + id
			if _, ok := s.group[name]; ok {
				return nil, alreadyExists("Reservation group %s already exists", name)
			}
			g.Name, g.CreationTime, g.UpdateTime = name, now, now
			s.group[name] = g
			return g, nil
		case http.MethodGet:
			var names []string
			for n := range s.group {
				if strings.HasPrefix(n, req.parent+"/reservationGroups/") {
					names = append(names, n)
				}
			}
			pg, next, err := page(names, req.query)
			if err != nil {
				return nil, err
			}
			out := &reservationpb.ListReservationGroupsResponse{NextPageToken: next}
			for _, n := range pg {
				out.ReservationGroups = append(out.ReservationGroups, s.group[n])
			}
			return out, nil
		}
		return nil, methodNotAllowed(req)
	}
	name := req.parent + "/reservationGroups/" + req.id
	g, ok := s.group[name]
	if !ok || req.verb != "" {
		return nil, notFoundErr("Reservation group %s not found", name)
	}
	switch req.method {
	case http.MethodGet:
		return g, nil
	case http.MethodPatch:
		// A group has no field but its name, which is its identity: a
		// patch changes nothing, and a mask naming any field is refused.
		in := &reservationpb.ReservationGroup{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		for _, f := range strings.Split(first(req.query["updateMask"]), ",") {
			if f = strings.TrimSpace(f); f != "" {
				return nil, invalidArg("Invalid update_mask path %q: a reservation group's %s cannot be updated", f, f)
			}
		}
		return g, nil
	case http.MethodDelete:
		// "A reservation group can be deleted only when it has no
		// members" (reservations-tasks).
		for _, rv := range s.reservation {
			if rv.GetReservationGroup() == name {
				return nil, failedPrecondition("Reservation group %s has reservations: remove them first", name)
			}
		}
		delete(s.group, name)
		return map[string]any{}, nil
	}
	return nil, methodNotAllowed(req)
}

func (s *bqReservations) callBI(req resvRequest) (any, error) {
	if req.id != "" || req.verb != "" {
		return nil, notFoundErr("Not found")
	}
	name := req.parent + "/biReservation"
	b, ok := s.bi[name]
	if !ok {
		b = &reservationpb.BiReservation{Name: name, UpdateTime: timestamppb.New(time.Unix(0, 0))}
	}
	switch req.method {
	case http.MethodGet:
		return b, nil
	case http.MethodPatch:
		in := &reservationpb.BiReservation{}
		if err := decodeInto(req.body, in); err != nil {
			return nil, err
		}
		paths := splitMask(first(req.query["updateMask"]))
		if len(paths) == 0 {
			paths = bodyKeys(req.body)
		}
		next := proto.Clone(b).(*reservationpb.BiReservation)
		for _, p := range paths {
			switch p {
			case "size":
				if in.GetSize() < 0 {
					return nil, invalidArg("size must not be negative")
				}
				next.Size = in.GetSize()
			case "preferred_tables", "preferredTables":
				next.PreferredTables = in.GetPreferredTables()
			case "name", "update_time", "updateTime":
			default:
				return nil, invalidArg("Invalid update_mask path %q", p)
			}
		}
		next.UpdateTime = timestamppb.New(s.clock())
		s.bi[name] = next
		return next, nil
	}
	return nil, methodNotAllowed(req)
}

// iam serves a reservation's getIamPolicy, setIamPolicy and
// testIamPermissions. The instance checks no bigquery.reservations.*
// permission, so each one asked is held.
func (s *bqReservations) iam(name string, req resvRequest) (any, error) {
	var body map[string]json.RawMessage
	if len(strings.TrimSpace(string(req.body))) > 0 {
		if err := json.Unmarshal(req.body, &body); err != nil {
			return nil, invalidArg("Invalid JSON payload received. %v", err)
		}
	}
	etag := func() string {
		return base64Etag(s.etag[name])
	}
	switch req.verb {
	case "getIamPolicy":
		p := map[string]any{"version": 1, "etag": etag()}
		if raw, ok := s.policy[name]; ok {
			_ = json.Unmarshal(raw, &p)
		}
		return p, nil
	case "setIamPolicy":
		var p map[string]any
		if raw, ok := body["policy"]; !ok || json.Unmarshal(raw, &p) != nil || p == nil {
			return nil, invalidArg("setIamPolicy requires a policy")
		}
		if et, _ := p["etag"].(string); et != "" && et != etag() {
			return nil, &resvErr{409, "ABORTED", "The policy's etag does not match the reservation's current policy: read it again with getIamPolicy"}
		}
		s.etag[name]++
		p["etag"] = etag()
		if _, ok := p["version"]; !ok {
			p["version"] = 1
		}
		s.policy[name], _ = json.Marshal(p)
		return p, nil
	default:
		var perms []string
		if raw, ok := body["permissions"]; ok {
			_ = json.Unmarshal(raw, &perms)
		}
		var held []string
		for _, p := range perms {
			if strings.HasPrefix(p, "bigquery.") {
				held = append(held, p)
			}
		}
		res := map[string]any{}
		if held != nil {
			res["permissions"] = held
		}
		return res, nil
	}
}

// base64Etag is a policy's etag, as Google's are: base64 bytes.
func base64Etag(n int64) string {
	return base64.StdEncoding.EncodeToString([]byte("BwY" + strconv.FormatInt(n, 10)))
}

// continuousReservation is the reservation the CONTINUOUS assignment of
// project (an ID) in location runs its continuous queries on: the
// project's own assignment, else one of its folders' (nearest first),
// else its organization's (ancestors, as Resource Manager gives them).
// ok is false when there is none.
func (s *bqReservations) continuousReservation(project, location string, ancestors []string) (rv *reservationpb.Reservation, ok bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	location = reservationLocation(location)
	for _, who := range append([]string{"projects/" + project}, ancestors...) {
		key := s.assigneeKey(who)
		var names []string
		for n, a := range s.assignment {
			if a.GetJobType() == reservationpb.Assignment_CONTINUOUS && s.assigneeKey(a.GetAssignee()) == key &&
				strings.Contains(n, "/locations/"+location+"/") {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			resName := n[:strings.Index(n, "/assignments/")]
			if r, ok := s.reservation[resName]; ok {
				return proto.Clone(r).(*reservationpb.Reservation), true
			}
		}
	}
	return nil, false
}

// editionRank orders the editions for upgradeEdition: an unspecified
// edition and STANDARD lowest, then ENTERPRISE, then ENTERPRISE_PLUS.
func editionRank(e reservationpb.Edition) int {
	switch e {
	case reservationpb.Edition_ENTERPRISE:
		return 1
	case reservationpb.Edition_ENTERPRISE_PLUS:
		return 2
	}
	return 0
}

// asEnterprise is an edition, EDITION_UNSPECIFIED being ENTERPRISE ("Default
// value, which will be treated as ENTERPRISE").
func asEnterprise(e reservationpb.Edition) reservationpb.Edition {
	if e == reservationpb.Edition_EDITION_UNSPECIFIED {
		return reservationpb.Edition_ENTERPRISE
	}
	return e
}

// flexibleSlots is, per edition, the baseline slots of a location's
// reservations beyond its ACTIVE commitments' slots: what is billed as
// pay-as-you-go capacity.
func flexibleSlots(reservations map[string]*reservationpb.Reservation, commitments map[string]*reservationpb.CapacityCommitment, parent string) map[reservationpb.Edition]int64 {
	base, committed := map[reservationpb.Edition]int64{}, map[reservationpb.Edition]int64{}
	for n, rv := range reservations {
		if strings.HasPrefix(n, parent+"/reservations/") {
			base[asEnterprise(rv.GetEdition())] += rv.GetSlotCapacity()
		}
	}
	for n, c := range commitments {
		if strings.HasPrefix(n, parent+"/capacityCommitments/") && c.GetState() == reservationpb.CapacityCommitment_ACTIVE {
			committed[asEnterprise(c.GetEdition())] += c.GetSlotCount()
		}
	}
	out := map[reservationpb.Edition]int64{}
	for e, b := range base {
		if b > committed[e] {
			out[e] = b - committed[e]
		}
	}
	return out
}

// upgradeEdition answers projects.locations.upgradeEdition, as the
// method's reference documents it: the reservations and capacity
// commitments named, which must exist under the parent (commitments
// ACTIVE), move to targetEdition, ENTERPRISE or ENTERPRISE_PLUS
// (EDITION_UNSPECIFIED is ENTERPRISE); never down nor to the edition they
// have; every reservation of a reservation group is upgraded together;
// and the upgrade may not raise the flexible (uncommitted) slots summed
// over the original and target editions. Otherwise FAILED_PRECONDITION.
// Nothing changes unless all of it can.
func (s *bqReservations) upgradeEdition(req resvRequest) (any, error) {
	var in struct {
		CapacityCommitments []string `json:"capacityCommitments"`
		Reservations        []string `json:"reservations"`
		TargetEdition       string   `json:"targetEdition"`
	}
	if len(strings.TrimSpace(string(req.body))) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(req.body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, invalidArg("Invalid JSON payload received. %v", err)
		}
	}
	target := reservationpb.Edition_ENTERPRISE
	switch in.TargetEdition {
	case "", "EDITION_UNSPECIFIED", "ENTERPRISE":
	case "ENTERPRISE_PLUS":
		target = reservationpb.Edition_ENTERPRISE_PLUS
	case "STANDARD":
		return nil, invalidArg("Invalid target edition STANDARD: the supported values are ENTERPRISE and ENTERPRISE_PLUS")
	default:
		return nil, invalidArg("Invalid value at 'target_edition', %q", in.TargetEdition)
	}
	if len(in.Reservations) == 0 && len(in.CapacityCommitments) == 0 {
		return nil, invalidArg("Name at least one reservation or capacity commitment to upgrade")
	}
	reservations := map[string]*reservationpb.Reservation{}
	for n, rv := range s.reservation {
		reservations[n] = proto.Clone(rv).(*reservationpb.Reservation)
	}
	commitments := map[string]*reservationpb.CapacityCommitment{}
	for n, c := range s.commitment {
		commitments[n] = proto.Clone(c).(*reservationpb.CapacityCommitment)
	}
	original := map[reservationpb.Edition]bool{}
	upgraded := map[string]bool{}
	var outR []*reservationpb.Reservation
	for _, n := range in.Reservations {
		n = s.canonicalName(n)
		if !strings.HasPrefix(n, req.parent+"/reservations/") || strings.Count(strings.TrimPrefix(n, req.parent+"/reservations/"), "/") != 0 {
			return nil, invalidArg("Reservation %s is not under %s", n, req.parent)
		}
		rv, ok := reservations[n]
		if !ok {
			return nil, notFoundErr("Reservation %s not found", n)
		}
		if editionRank(rv.GetEdition()) >= editionRank(target) && rv.GetEdition() != reservationpb.Edition_EDITION_UNSPECIFIED {
			return nil, failedPrecondition("Reservation %s is %s: an edition can only be upgraded, to a higher one than %s", n, rv.GetEdition(), target)
		}
		original[asEnterprise(rv.GetEdition())] = true
		rv.Edition, rv.UpdateTime = target, timestamppb.New(s.now())
		upgraded[n] = true
		outR = append(outR, rv)
	}
	for _, rv := range outR {
		if g := rv.GetReservationGroup(); g != "" {
			for n, other := range reservations {
				if other.GetReservationGroup() == g && !upgraded[n] {
					return nil, failedPrecondition("Reservation %s is in reservation group %s with %s: the group's reservations are upgraded together", rv.GetName(), g, n)
				}
			}
		}
	}
	var outC []*reservationpb.CapacityCommitment
	for _, n := range in.CapacityCommitments {
		n = s.canonicalName(n)
		if !strings.HasPrefix(n, req.parent+"/capacityCommitments/") {
			return nil, invalidArg("Capacity commitment %s is not under %s", n, req.parent)
		}
		c, ok := commitments[n]
		if !ok {
			return nil, notFoundErr("Capacity commitment %s not found", n)
		}
		if c.GetState() != reservationpb.CapacityCommitment_ACTIVE {
			return nil, failedPrecondition("Capacity commitment %s is %s: only an ACTIVE commitment is upgraded", n, c.GetState())
		}
		if editionRank(c.GetEdition()) >= editionRank(target) && c.GetEdition() != reservationpb.Edition_EDITION_UNSPECIFIED {
			return nil, failedPrecondition("Capacity commitment %s is %s: an edition can only be upgraded, to a higher one than %s", n, c.GetEdition(), target)
		}
		original[asEnterprise(c.GetEdition())] = true
		c.Edition = target
		outC = append(outC, c)
	}
	before, after := flexibleSlots(s.reservation, s.commitment, req.parent), flexibleSlots(reservations, commitments, req.parent)
	original[target] = true
	var sumBefore, sumAfter int64
	for e := range original {
		sumBefore += before[e]
		sumAfter += after[e]
	}
	if sumAfter > sumBefore {
		return nil, failedPrecondition("The upgrade raises the flexible slots across the original and target editions from %d to %d: "+
			"upgrade the reservations' capacity commitments with them", sumBefore, sumAfter)
	}
	s.reservation, s.commitment = reservations, commitments
	out := map[string]any{}
	if len(outR) > 0 {
		var rs []json.RawMessage
		for _, rv := range outR {
			b, _ := protojson.Marshal(rv)
			rs = append(rs, b)
		}
		out["reservations"] = rs
	}
	if len(outC) > 0 {
		var cs []json.RawMessage
		for _, c := range outC {
			b, _ := protojson.Marshal(c)
			cs = append(cs, b)
		}
		out["capacityCommitments"] = cs
	}
	return out, nil
}

// checkSecondary holds a reservation's secondary location to managed
// disaster recovery's rules
// (https://docs.cloud.google.com/bigquery/docs/managed-disaster-recovery,
// read 2026-11-23): an Enterprise Plus reservation's only, and another
// location than its primary.
func checkSecondary(edition reservationpb.Edition, primary, secondary string) error {
	if secondary == "" {
		return nil
	}
	if edition != reservationpb.Edition_ENTERPRISE_PLUS {
		return invalidArg("A secondary location needs an ENTERPRISE_PLUS reservation: managed disaster recovery is an Enterprise Plus feature")
	}
	if strings.EqualFold(secondary, primary) {
		return invalidArg("The secondary location %s is the reservation's primary location", secondary)
	}
	return nil
}

// iamMethod checks an IAM verb's HTTP method as bigqueryreservation v1
// binds it: getIamPolicy is a GET (its options.requestedPolicyVersion a
// query parameter), setIamPolicy and testIamPermissions POSTs.
func iamMethod(req resvRequest) error {
	want := http.MethodPost
	if req.verb == "getIamPolicy" {
		want = http.MethodGet
	}
	if req.method != want {
		return methodNotAllowed(req)
	}
	return nil
}
