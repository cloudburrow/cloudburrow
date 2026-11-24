package main

// BigQuery's Capacity management page (#2193), as
// https://docs.cloud.google.com/bigquery/docs/reservations-tasks,
// https://docs.cloud.google.com/bigquery/docs/reservations-assignments and
// https://docs.cloud.google.com/bigquery/docs/reservations-commitments
// describe it (read 2026-11-23): "In the Google Cloud console, go to the
// BigQuery page. In the navigation menu, click Capacity management", whose
// Slot reservations tab lists the reservations (Create reservation:
// Reservation name, Location, Edition, Max reservation size, Baseline
// slots, Ignore idle slots), each with its assignments ("Create
// assignment": the organization, folder or project and the Job Type,
// CONTINUOUS among them), Edit and Delete; and Slot commitments (Create
// commitment: Location, Edition, Commitment duration, Slots, Renewal plan;
// Delete). Every change is one call of the Reservation API the BigQuery
// front serves (internal/bigqueryfront/reservations.go), through Google's
// REST client.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	reservation "cloud.google.com/go/bigquery/reservation/apiv1"
	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// capacityLocations are the locations the pages list and create in.
var capacityLocations = []string{"US", "EU", "us-central1", "us-east1", "europe-west1", "asia-northeast1"}

// capacityJobTypes are the assignment job types the console offers.
var capacityJobTypes = []string{"QUERY", "PIPELINE", "ML_EXTERNAL", "BACKGROUND", "CONTINUOUS"}

const (
	actCreateAssignment = "createassignment"
	actDeleteAssignment = "deleteassignment"
	actEditReservation  = "editreservation"
)

// bqCapacityProvider is the Slot reservations tab.
type bqCapacityProvider struct{ bq bigqueryProvider }

// bqCommitmentsProvider is the Slot commitments tab.
type bqCommitmentsProvider struct{ bq bigqueryProvider }

func (bqCapacityProvider) ID() string       { return "bigquery-capacity" }
func (bqCapacityProvider) Title() string    { return "Capacity management" }
func (bqCommitmentsProvider) ID() string    { return "bigquery-commitments" }
func (bqCommitmentsProvider) Title() string { return "Slot commitments" }

func reservationAPI(ctx context.Context, endpoint string) (*reservation.Client, error) {
	if endpoint == "" {
		return nil, errors.New("BigQuery is not running on this instance")
	}
	return reservation.NewRESTClient(ctx, option.WithEndpoint("http://"+endpoint), option.WithoutAuthentication())
}

func (p bqCapacityProvider) project(project string) string {
	if project == "" {
		return p.bq.project
	}
	return project
}

func (p bqCapacityProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Edition", "Baseline slots", "Max reservation size", "Ignore idle slots", "Assignments"},
		NameColumn: "Reservation name", Noun: "reservations"}
	project = p.project(project)
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()
	for _, loc := range capacityLocations {
		it := c.ListReservations(ctx, &reservationpb.ListReservationsRequest{Parent: "projects/" + project + "/locations/" + loc})
		for {
			rv, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return console.Listing{}, errors.New(consoleErr(err))
			}
			var jobs []string
			ait := c.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: rv.GetName()})
			for {
				a, err := ait.Next()
				if err != nil {
					break
				}
				jobs = append(jobs, a.GetJobType().String()+" "+a.GetAssignee())
			}
			listing.Items = append(listing.Items, console.Resource{Name: loc + "/" + lastSegment(rv.GetName()), Fields: map[string]string{
				"Location": loc, "Edition": rv.GetEdition().String(), "Baseline slots": strconv.FormatInt(rv.GetSlotCapacity(), 10),
				"Max reservation size": strconv.FormatInt(maxReservationSize(rv), 10), "Ignore idle slots": strconv.FormatBool(rv.GetIgnoreIdleSlots()),
				"Assignments": orDash(strings.Join(jobs, ", ")),
			}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

// maxReservationSize is the baseline and the autoscaling slots, the
// console's "Max reservation size".
func maxReservationSize(rv *reservationpb.Reservation) int64 {
	if rv.MaxSlots != nil && rv.GetMaxSlots() > 0 {
		return rv.GetMaxSlots()
	}
	return rv.GetSlotCapacity() + rv.GetAutoscale().GetMaxSlots()
}

func (p bqCapacityProvider) CreateForm() (string, []console.Field) {
	return "Create reservation", []console.Field{
		{Name: "id", Label: "Reservation name", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,62}[a-z0-9])?$`,
			Help: "Lower case letters, numbers and dashes; starts with a letter, does not end with a dash; at most 64 characters."},
		{Name: "location", Label: "Location", Type: "select", Required: true, Options: capacityLocations, Default: "US"},
		{Name: "edition", Label: "Edition", Type: "select", Required: true, Options: []string{"STANDARD", "ENTERPRISE", "ENTERPRISE_PLUS"}, Default: "ENTERPRISE",
			Help: "Continuous queries run on an Enterprise or Enterprise Plus reservation."},
		{Name: "max", Label: "Max reservation size", Type: "number", Required: true, Default: "100",
			Help: "The most slots the reservation uses: its baseline and its autoscaling, in multiples of 50."},
		{Name: "baseline", Label: "Baseline slots", Type: "number", Default: "0",
			Help: "Slots always allocated to the reservation, in multiples of 50. A Standard edition reservation has none."},
		{Name: "ignoreIdle", Label: "Ignore idle slots", Type: "checkbox", Section: "Advanced settings",
			Help: "Queries in this reservation use only the reservation's own slots, not idle slots of other reservations."},
	}
}

func atoi64(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

func (p bqCapacityProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	max, err := atoi64(v["max"])
	if err != nil {
		return "", fmt.Errorf("Max reservation size: %v", err)
	}
	base, err := atoi64(v["baseline"])
	if err != nil {
		return "", fmt.Errorf("Baseline slots: %v", err)
	}
	if max < base {
		return "", errors.New("the max reservation size is at least the baseline slots")
	}
	rv := &reservationpb.Reservation{SlotCapacity: base, Edition: reservationpb.Edition(reservationpb.Edition_value[v["edition"]]),
		IgnoreIdleSlots: checkboxOn(v["ignoreIdle"])}
	if max > base {
		rv.Autoscale = &reservationpb.Reservation_Autoscale{MaxSlots: max - base}
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return "", err
	}
	defer c.Close()
	got, err := c.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: "projects/" + project + "/locations/" + v["location"],
		ReservationId: v["id"], Reservation: rv})
	if err != nil {
		return "", errors.New(consoleErr(err))
	}
	return v["location"] + "/" + lastSegment(got.GetName()), nil
}

// reservationName is a row's ("location/id") reservation.
func (p bqCapacityProvider) reservationName(project, row string) (string, error) {
	loc, id, ok := strings.Cut(row, "/")
	if !ok || loc == "" || id == "" {
		return "", fmt.Errorf("%q is not a reservation", row)
	}
	return "projects/" + p.project(project) + "/locations/" + loc + "/reservations/" + id, nil
}

func (p bqCapacityProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	if len(path) == 2 {
		return p.assignmentDetail(ctx, project, path)
	}
	name, err := p.reservationName(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	rv, err := c.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	assignments := console.Listing{Columns: []string{"Assignee", "Job type", "State"}, NameColumn: "Assignment ID", Noun: "assignments"}
	it := c.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: name})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return console.Detail{Unavailable: consoleErr(err)}, nil
		}
		assignments.Items = append(assignments.Items, console.Resource{Name: lastSegment(a.GetName()), Fields: map[string]string{
			"Assignee": a.GetAssignee(), "Job type": a.GetJobType().String(), "State": a.GetState().String()}})
	}
	assignments.Total = len(assignments.Items)
	assignments.RowsOpenable = true
	return console.Detail{
		Summary: []console.Property{
			{Label: "Reservation name", Value: lastSegment(rv.GetName())},
			{Label: "Location", Value: rv.GetPrimaryLocation()},
			{Label: "Edition", Value: rv.GetEdition().String()},
			{Label: "Secondary location", Value: orDash(rv.GetSecondaryLocation())},
			{Label: "Reservation group", Value: orDash(lastSegment(rv.GetReservationGroup()))},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
				{Label: "Baseline slots", Value: strconv.FormatInt(rv.GetSlotCapacity(), 10)},
				{Label: "Max reservation size", Value: strconv.FormatInt(maxReservationSize(rv), 10)},
				{Label: "Autoscale slots", Value: strconv.FormatInt(rv.GetAutoscale().GetMaxSlots(), 10)},
				{Label: "Ignore idle slots", Value: strconv.FormatBool(rv.GetIgnoreIdleSlots())},
				{Label: "Creation time", Value: rv.GetCreationTime().AsTime().UTC().Format("Jan 2, 2006, 3:04:05 PM UTC")},
			}}}},
			{ID: "assignments", Label: "Assignments", Listing: assignments},
		},
	}, nil
}

func (p bqCapacityProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) == 2 {
		return p.assignmentActions(ctx, project, path)
	}
	if len(path) != 1 {
		return nil
	}
	acts := []console.Action{
		{ID: actCreateAssignment, Label: "Create assignment", Title: "Create assignment", Fields: []console.Field{
			{Name: "assigneeType", Label: "Assignee type", Type: "select", Required: true, Options: []string{"projects", "folders", "organizations"}, Default: "projects"},
			{Name: "assignee", Label: "Select an organization, folder, or project", Type: "text", Required: true, Default: p.project(project),
				Help: "The project ID, folder number or organization number whose jobs run on this reservation."},
			{Name: "jobType", Label: "Job Type", Type: "select", Required: true, Options: capacityJobTypes, Default: "QUERY",
				Help: "CONTINUOUS runs the assignee's continuous queries on this reservation. A reservation with a CONTINUOUS assignment has no other job type."},
		}},
		{ID: actEditReservation, Label: "Edit", Title: "Edit reservation", Fields: []console.Field{
			{Name: "max", Label: "Max reservation size", Type: "number", Required: true},
			{Name: "baseline", Label: "Baseline slots", Type: "number", Required: true},
		}},
	}
	if c, err := reservationAPI(ctx, p.bq.endpoint); err == nil {
		defer c.Close()
		if name, err := p.reservationName(project, path[0]); err == nil {
			if rv, err := c.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: name}); err == nil {
				acts[1].Fields[0].Default = strconv.FormatInt(maxReservationSize(rv), 10)
				acts[1].Fields[1].Default = strconv.FormatInt(rv.GetSlotCapacity(), 10)
				acts[1].Fields = append(acts[1].Fields, p.groupField(ctx, c, project, path[0], rv.GetReservationGroup()))
				if rv.GetEdition() == reservationpb.Edition_ENTERPRISE_PLUS {
					acts[1].Fields = append(acts[1].Fields, console.Field{Name: "secondary", Label: "Secondary location", Type: "text",
						Default: rv.GetSecondaryLocation(), Help: "Managed disaster recovery's secondary location; empty for none."})
				}
				acts = append(acts, p.reservationExtraActions(ctx, c, project, path[0], rv)...)
			}
			var ids []string
			it := c.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: name})
			for {
				a, err := it.Next()
				if err != nil {
					break
				}
				ids = append(ids, lastSegment(a.GetName()))
			}
			if len(ids) > 0 {
				acts = append(acts, console.Action{ID: actDeleteAssignment, Label: "Delete assignment", Destructive: true, Fields: []console.Field{
					{Name: "assignment", Label: "Assignment", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
			}
		}
	}
	return acts
}

func (p bqCapacityProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) == 2 {
		return p.actAssignment(ctx, project, path, action, v)
	}
	if len(path) != 1 {
		return fmt.Errorf("no action %q here", action)
	}
	name, err := p.reservationName(project, path[0])
	if err != nil {
		return err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	switch action {
	case actCreateAssignment:
		jt, ok := reservationpb.Assignment_JobType_value[v["jobType"]]
		if !ok {
			return fmt.Errorf("unknown job type %q", v["jobType"])
		}
		_, err = c.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: name, Assignment: &reservationpb.Assignment{
			Assignee: v["assigneeType"] + "/" + strings.TrimSpace(v["assignee"]), JobType: reservationpb.Assignment_JobType(jt)}})
	case actDeleteAssignment:
		err = c.DeleteAssignment(ctx, &reservationpb.DeleteAssignmentRequest{Name: name + "/assignments/" + v["assignment"]})
	case actEditReservation:
		max, err1 := atoi64(v["max"])
		base, err2 := atoi64(v["baseline"])
		if err1 != nil || err2 != nil || max < base {
			return errors.New("the max reservation size and the baseline slots are numbers, the size at least the baseline")
		}
		rv := &reservationpb.Reservation{Name: name, SlotCapacity: base, Autoscale: &reservationpb.Reservation_Autoscale{MaxSlots: max - base}}
		paths := []string{"slot_capacity", "autoscale.max_slots"}
		if g, ok := v["group"]; ok {
			rv.ReservationGroup, paths = p.groupName(project, path[0], g), append(paths, "reservation_group")
		}
		if sl, ok := v["secondary"]; ok {
			rv.SecondaryLocation, paths = strings.TrimSpace(sl), append(paths, "secondary_location")
		}
		_, err = c.UpdateReservation(ctx, &reservationpb.UpdateReservationRequest{Reservation: rv,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	case actFailover, actUpgradeEdition:
		return p.actReservationExtra(ctx, project, path[0], action, v)
	default:
		return fmt.Errorf("no action %q here", action)
	}
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

func (p bqCapacityProvider) Delete(ctx context.Context, project, row string) error {
	name, err := p.reservationName(project, row)
	if err != nil {
		return err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.DeleteReservation(ctx, &reservationpb.DeleteReservationRequest{Name: name}); err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

// Slot commitments.

func (p bqCommitmentsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Edition", "Slots", "Commitment plan", "Renewal plan", "State", "End time"},
		NameColumn: "Commitment ID", Noun: "commitments"}
	project = bqCapacityProvider(p).project(project)
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()
	for _, loc := range capacityLocations {
		it := c.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: "projects/" + project + "/locations/" + loc})
		for {
			cm, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return console.Listing{}, errors.New(consoleErr(err))
			}
			listing.Items = append(listing.Items, console.Resource{Name: loc + "/" + lastSegment(cm.GetName()), Fields: map[string]string{
				"Location": loc, "Edition": cm.GetEdition().String(), "Slots": strconv.FormatInt(cm.GetSlotCount(), 10),
				"Commitment plan": cm.GetPlan().String(), "Renewal plan": cm.GetRenewalPlan().String(), "State": cm.GetState().String(),
				"End time": cm.GetCommitmentEndTime().AsTime().UTC().Format("Jan 2, 2006, 3:04:05 PM UTC"),
			}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (p bqCommitmentsProvider) CreateForm() (string, []console.Field) {
	return "Create commitment", []console.Field{
		{Name: "location", Label: "Location", Type: "select", Required: true, Options: capacityLocations, Default: "US"},
		{Name: "edition", Label: "Edition", Type: "select", Required: true, Options: []string{"ENTERPRISE", "ENTERPRISE_PLUS"}, Default: "ENTERPRISE"},
		{Name: "plan", Label: "Commitment duration", Type: "select", Required: true, Options: []string{"ANNUAL", "THREE_YEAR"}, Default: "ANNUAL",
			Help: "An edition's commitment is for one year or three."},
		{Name: "slots", Label: "Slots", Type: "number", Required: true, Default: "100", Help: "In multiples of 50."},
		{Name: "renewal", Label: "Renewal plan", Type: "select", Options: []string{"NONE", "ANNUAL"}, Default: "NONE"},
	}
}

func (p bqCommitmentsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = bqCapacityProvider(p).project(project)
	slots, err := atoi64(v["slots"])
	if err != nil {
		return "", fmt.Errorf("Slots: %v", err)
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return "", err
	}
	defer c.Close()
	cm, err := c.CreateCapacityCommitment(ctx, &reservationpb.CreateCapacityCommitmentRequest{Parent: "projects/" + project + "/locations/" + v["location"],
		CapacityCommitment: &reservationpb.CapacityCommitment{SlotCount: slots,
			Plan:        reservationpb.CapacityCommitment_CommitmentPlan(reservationpb.CapacityCommitment_CommitmentPlan_value[v["plan"]]),
			RenewalPlan: reservationpb.CapacityCommitment_CommitmentPlan(reservationpb.CapacityCommitment_CommitmentPlan_value[v["renewal"]]),
			Edition:     reservationpb.Edition(reservationpb.Edition_value[v["edition"]])}})
	if err != nil {
		return "", errors.New(consoleErr(err))
	}
	return v["location"] + "/" + lastSegment(cm.GetName()), nil
}

func (p bqCommitmentsProvider) Delete(ctx context.Context, project, row string) error {
	loc, id, ok := strings.Cut(row, "/")
	if !ok {
		return fmt.Errorf("%q is not a commitment", row)
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	name := "projects/" + bqCapacityProvider(p).project(project) + "/locations/" + loc + "/capacityCommitments/" + id
	if err := c.DeleteCapacityCommitment(ctx, &reservationpb.DeleteCapacityCommitmentRequest{Name: name}); err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

var (
	_ console.Creator   = bqCapacityProvider{}
	_ console.PathActor = bqCapacityProvider{}
	_ console.Deleter   = bqCapacityProvider{}
	_ console.Creator   = bqCommitmentsProvider{}
	_ console.Deleter   = bqCommitmentsProvider{}
)
