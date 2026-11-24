package main

// The rest of BigQuery's Capacity management (#2193), so every method of
// the Reservation API the front serves is reachable from the console:
//   - an assignment's page (Edit, Move, Delete and its Permissions);
//   - a reservation's Failover, Upgrade edition, reservation group and
//     secondary location (Edit) and Permissions;
//   - a commitment's page (Edit renewal plan, Split, Merge);
//   - Reservation groups, a tab of the page;
//   - BI Engine, BigQuery's page of its BI reservations.
// Each is one call of the API through Google's REST client.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	bqresv "google.golang.org/api/bigqueryreservation/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	reservation "cloud.google.com/go/bigquery/reservation/apiv1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	actFailover         = "failover"
	actUpgradeEdition   = "upgradeedition"
	actEditAssignment   = "editassignment"
	actMoveAssignment   = "moveassignment"
	actRenewalPlan      = "renewal"
	actSplitCommitment  = "split"
	actMergeCommitments = "merge"
	noReservationGroup  = "(none)"
	capacityPermissions = "Reservations, assignments and their policies are stored; the instance checks none of their permissions."
)

// assignmentName is the assignment at a reservation page's path.
func (p bqCapacityProvider) assignmentName(project string, path []string) (string, error) {
	rv, err := p.reservationName(project, path[0])
	if err != nil {
		return "", err
	}
	return rv + "/assignments/" + path[1], nil
}

func (p bqCapacityProvider) getAssignment(ctx context.Context, c *reservation.Client, name string) (*reservationpb.Assignment, error) {
	parent := name[:strings.LastIndex(name, "/assignments/")]
	it := c.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: parent})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return nil, fmt.Errorf("assignment %s not found", lastSegment(name))
		}
		if err != nil {
			return nil, errors.New(consoleErr(err))
		}
		if a.GetName() == name {
			return a, nil
		}
	}
}

func (p bqCapacityProvider) assignmentDetail(ctx context.Context, project string, path []string) (console.Detail, error) {
	name, err := p.assignmentName(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	a, err := p.getAssignment(ctx, c, name)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Assignment ID", Value: lastSegment(a.GetName())},
			{Label: "Reservation", Value: path[0]},
		},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Assignee", Value: a.GetAssignee()},
			{Label: "Job type", Value: a.GetJobType().String()},
			{Label: "State", Value: a.GetState().String()},
			{Label: "Gemini in BigQuery", Value: strconv.FormatBool(a.GetEnableGeminiInBigquery())},
		}}}}},
	}, nil
}

func (p bqCapacityProvider) assignmentActions(ctx context.Context, project string, path []string) []console.Action {
	acts := []console.Action{
		{ID: actEditAssignment, Label: "Edit", Title: "Edit assignment", Fields: []console.Field{
			{Name: "gemini", Label: "Enable Gemini in BigQuery", Type: "checkbox"}}},
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return acts
	}
	defer c.Close()
	if name, err := p.assignmentName(project, path); err == nil {
		if a, err := p.getAssignment(ctx, c, name); err == nil && a.GetEnableGeminiInBigquery() {
			acts[0].Fields[0].Default = "true"
		}
	}
	loc, _, _ := strings.Cut(path[0], "/")
	var others []string
	it := c.ListReservations(ctx, &reservationpb.ListReservationsRequest{Parent: "projects/" + p.project(project) + "/locations/" + loc})
	for rv, err := it.Next(); err == nil; rv, err = it.Next() {
		if id := lastSegment(rv.GetName()); loc+"/"+id != path[0] {
			others = append(others, id)
		}
	}
	if len(others) > 0 {
		acts = append(acts, console.Action{ID: actMoveAssignment, Label: "Move", Title: "Move assignment", Fields: []console.Field{
			{Name: "to", Label: "Destination reservation", Type: "select", Required: true, Options: others, Default: others[0]}}})
	}
	return append(acts, console.Action{ID: actDeleteAssignment, Label: "Delete assignment", Destructive: true, ConfirmStyle: console.DeleteConfirmClick})
}

func (p bqCapacityProvider) actAssignment(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	name, err := p.assignmentName(project, path)
	if err != nil {
		return err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	switch action {
	case actEditAssignment:
		_, err = c.UpdateAssignment(ctx, &reservationpb.UpdateAssignmentRequest{
			Assignment: &reservationpb.Assignment{Name: name, EnableGeminiInBigquery: checkboxOn(v["gemini"])},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enable_gemini_in_bigquery"}}})
	case actMoveAssignment:
		loc, _, _ := strings.Cut(path[0], "/")
		_, err = c.MoveAssignment(ctx, &reservationpb.MoveAssignmentRequest{Name: name,
			DestinationId: "projects/" + p.project(project) + "/locations/" + loc + "/reservations/" + v["to"]})
	case actDeleteAssignment:
		err = c.DeleteAssignment(ctx, &reservationpb.DeleteAssignmentRequest{Name: name})
	default:
		return fmt.Errorf("no action %q here", action)
	}
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

// groupField is the Edit dialog's reservation group, chosen from the
// location's groups.
func (p bqCapacityProvider) groupField(ctx context.Context, c *reservation.Client, project, row, current string) console.Field {
	loc, _, _ := strings.Cut(row, "/")
	opts := []string{noReservationGroup}
	it := c.ListReservationGroups(ctx, &reservationpb.ListReservationGroupsRequest{Parent: "projects/" + p.project(project) + "/locations/" + loc})
	for g, err := it.Next(); err == nil; g, err = it.Next() {
		opts = append(opts, lastSegment(g.GetName()))
	}
	def := noReservationGroup
	if current != "" {
		def = lastSegment(current)
	}
	return console.Field{Name: "group", Label: "Reservation group", Type: "select", Options: opts, Default: def}
}

func (p bqCapacityProvider) groupName(project, row, group string) string {
	if group == "" || group == noReservationGroup {
		return ""
	}
	loc, _, _ := strings.Cut(row, "/")
	return "projects/" + p.project(project) + "/locations/" + loc + "/reservationGroups/" + group
}

func (p bqCapacityProvider) reservationExtraActions(ctx context.Context, c *reservation.Client, project, row string, rv *reservationpb.Reservation) []console.Action {
	var acts []console.Action
	if rv.GetSecondaryLocation() != "" {
		// Nothing is discarded: the locations swap, and Failover again
		// swaps them back.
		acts = append(acts, console.Action{ID: actFailover, Label: "Failover"})
	}
	if rv.GetEdition() != reservationpb.Edition_ENTERPRISE_PLUS {
		targets := []string{"ENTERPRISE_PLUS"}
		if editionRankOf(rv.GetEdition()) == 0 {
			targets = []string{"ENTERPRISE", "ENTERPRISE_PLUS"}
		}
		loc, _, _ := strings.Cut(row, "/")
		var commitments []string
		it := c.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: "projects/" + p.project(project) + "/locations/" + loc})
		for cm, err := it.Next(); err == nil; cm, err = it.Next() {
			if cm.GetState() == reservationpb.CapacityCommitment_ACTIVE {
				commitments = append(commitments, lastSegment(cm.GetName()))
			}
		}
		help := "The ACTIVE commitments of " + loc + " to upgrade with it, by ID, separated by commas: an upgrade may not raise the slots not covered by commitments."
		if len(commitments) > 0 {
			help += " This location's: " + strings.Join(commitments, ", ") + "."
		}
		acts = append(acts, console.Action{ID: actUpgradeEdition, Label: "Upgrade edition", Fields: []console.Field{
			{Name: "target", Label: "Target edition", Type: "select", Required: true, Options: targets, Default: targets[0]},
			{Name: "commitments", Label: "Capacity commitments", Type: "text", Help: help}}})
	}
	return acts
}

func editionRankOf(e reservationpb.Edition) int {
	switch e {
	case reservationpb.Edition_ENTERPRISE:
		return 1
	case reservationpb.Edition_ENTERPRISE_PLUS:
		return 2
	}
	return 0
}

func (p bqCapacityProvider) actReservationExtra(ctx context.Context, project, row, action string, v map[string]string) error {
	name, err := p.reservationName(project, row)
	if err != nil {
		return err
	}
	switch action {
	case actFailover:
		c, err := reservationAPI(ctx, p.bq.endpoint)
		if err != nil {
			return err
		}
		defer c.Close()
		if _, err := c.FailoverReservation(ctx, &reservationpb.FailoverReservationRequest{Name: name}); err != nil {
			return errors.New(consoleErr(err))
		}
		return nil
	case actUpgradeEdition:
		// Google's Go gapic client has no UpgradeEdition: its discovery
		// client, google.golang.org/api/bigqueryreservation/v1, has.
		if p.bq.endpoint == "" {
			return errors.New("BigQuery is not running on this instance")
		}
		svc, err := bqresv.NewService(ctx, option.WithEndpoint("http://"+p.bq.endpoint+"/"), option.WithoutAuthentication())
		if err != nil {
			return err
		}
		loc, _, _ := strings.Cut(row, "/")
		parent := "projects/" + p.project(project) + "/locations/" + loc
		req := &bqresv.UpgradeEditionRequest{Reservations: []string{name}, TargetEdition: v["target"]}
		for _, id := range strings.Split(v["commitments"], ",") {
			if id = strings.TrimSpace(id); id != "" {
				req.CapacityCommitments = append(req.CapacityCommitments, parent+"/capacityCommitments/"+id)
			}
		}
		if _, err := svc.Projects.Locations.UpgradeEdition(parent, req).Context(ctx).Do(); err != nil {
			return errors.New(consoleErr(err))
		}
		return nil
	}
	return fmt.Errorf("no action %q here", action)
}

// The Permissions tab of a reservation and of an assignment.

func (p bqCapacityProvider) iamResource(project string, path []string) (string, error) {
	switch len(path) {
	case 1:
		return p.reservationName(project, path[0])
	case 2:
		return p.assignmentName(project, path)
	}
	return "", errors.New("only a reservation or an assignment has an IAM policy")
}

func (p bqCapacityProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 && len(path) != 2 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "bigquery-reservation-api-and-continuous-queries-2193",
		RoleHelp: roleHelp("BigQuery Reservation", "roles/bigquery.resourceAdmin")}
}

func (p bqCapacityProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	resource, err := p.iamResource(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Policy{}, err
	}
	defer c.Close()
	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		return console.Policy{}, errors.New(consoleErr(err))
	}
	return policyFromProto(pol), nil
}

func (p bqCapacityProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	resource, err := p.iamResource(project, path)
	if err != nil {
		return err
	}
	etag, err := base64.StdEncoding.DecodeString(pol.Etag)
	if err != nil {
		return fmt.Errorf("the policy's etag: %v", err)
	}
	out, err := policyToProto(pol, etag)
	if err != nil {
		return err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: out}); err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

func (p bqCapacityProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	resource, err := p.iamResource(project, path)
	if err != nil {
		return nil, err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: resource, Permissions: perms})
	if err != nil {
		return nil, errors.New(consoleErr(err))
	}
	return res.GetPermissions(), nil
}

// A commitment's page.

func (p bqCommitmentsProvider) commitmentName(project, row string) (string, error) {
	loc, id, ok := strings.Cut(row, "/")
	if !ok || loc == "" || id == "" {
		return "", fmt.Errorf("%q is not a commitment", row)
	}
	return "projects/" + bqCapacityProvider(p).project(project) + "/locations/" + loc + "/capacityCommitments/" + id, nil
}

func (p bqCommitmentsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	name, err := p.commitmentName(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	cm, err := c.GetCapacityCommitment(ctx, &reservationpb.GetCapacityCommitmentRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	const layout = "Jan 2, 2006, 3:04:05 PM UTC"
	return console.Detail{
		Summary: []console.Property{
			{Label: "Commitment ID", Value: lastSegment(cm.GetName())},
			{Label: "Edition", Value: cm.GetEdition().String()},
			{Label: "State", Value: cm.GetState().String()},
		},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Slots", Value: strconv.FormatInt(cm.GetSlotCount(), 10)},
			{Label: "Commitment plan", Value: cm.GetPlan().String()},
			{Label: "Renewal plan", Value: cm.GetRenewalPlan().String()},
			{Label: "Start time", Value: cm.GetCommitmentStartTime().AsTime().UTC().Format(layout)},
			{Label: "End time", Value: cm.GetCommitmentEndTime().AsTime().UTC().Format(layout)},
		}}}}},
	}, nil
}

func (p bqCommitmentsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	acts := []console.Action{
		{ID: actRenewalPlan, Label: "Edit", Title: "Edit renewal plan", Fields: []console.Field{
			{Name: "renewal", Label: "Renewal plan", Type: "select", Required: true, Options: []string{"NONE", "ANNUAL"}, Default: "NONE",
				Help: "What the commitment becomes at its end: an edition's renews for a year, or ends."}}},
		{ID: actSplitCommitment, Label: "Split", Title: "Split commitment", Fields: []console.Field{
			{Name: "slots", Label: "Slots to split off", Type: "number", Required: true, Default: "50", Help: "In multiples of 50, fewer than the commitment's."}}},
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return acts
	}
	defer c.Close()
	name, err := p.commitmentName(project, path[0])
	if err != nil {
		return acts
	}
	if cm, err := c.GetCapacityCommitment(ctx, &reservationpb.GetCapacityCommitmentRequest{Name: name}); err == nil {
		acts[0].Fields[0].Default = cm.GetRenewalPlan().String()
		if cm.GetRenewalPlan() == reservationpb.CapacityCommitment_COMMITMENT_PLAN_UNSPECIFIED {
			acts[0].Fields[0].Default = "NONE"
		}
	}
	var others []string
	it := c.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: name[:strings.LastIndex(name, "/capacityCommitments/")]})
	for cm, err := it.Next(); err == nil; cm, err = it.Next() {
		if cm.GetName() != name {
			others = append(others, lastSegment(cm.GetName()))
		}
	}
	if len(others) > 0 {
		acts = append(acts, console.Action{ID: actMergeCommitments, Label: "Merge", Title: "Merge commitments", Fields: []console.Field{
			{Name: "with", Label: "Merge with", Type: "select", Required: true, Options: others, Default: others[0],
				Help: "A commitment of the same plan and edition; the two become one."}}})
	}
	return acts
}

func (p bqCommitmentsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no action %q here", action)
	}
	name, err := p.commitmentName(project, path[0])
	if err != nil {
		return err
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	switch action {
	case actRenewalPlan:
		plan := reservationpb.CapacityCommitment_CommitmentPlan(reservationpb.CapacityCommitment_CommitmentPlan_value[v["renewal"]])
		_, err = c.UpdateCapacityCommitment(ctx, &reservationpb.UpdateCapacityCommitmentRequest{
			CapacityCommitment: &reservationpb.CapacityCommitment{Name: name, RenewalPlan: plan},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"renewal_plan"}}})
	case actSplitCommitment:
		slots, perr := atoi64(v["slots"])
		if perr != nil {
			return fmt.Errorf("Slots: %v", perr)
		}
		_, err = c.SplitCapacityCommitment(ctx, &reservationpb.SplitCapacityCommitmentRequest{Name: name, SlotCount: slots})
	case actMergeCommitments:
		_, err = c.MergeCapacityCommitments(ctx, &reservationpb.MergeCapacityCommitmentsRequest{
			Parent: name[:strings.LastIndex(name, "/capacityCommitments/")], CapacityCommitmentIds: []string{lastSegment(name), v["with"]}})
	default:
		return fmt.Errorf("no action %q here", action)
	}
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

// Reservation groups, a tab of Capacity management.

type bqReservationGroupsProvider struct{ bq bigqueryProvider }

func (bqReservationGroupsProvider) ID() string    { return "bigquery-reservation-groups" }
func (bqReservationGroupsProvider) Title() string { return "Reservation groups" }

func (p bqReservationGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Reservations"}, NameColumn: "Reservation group", Noun: "reservation groups"}
	project = bqCapacityProvider(p).project(project)
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()
	for _, loc := range capacityLocations {
		parent := "projects/" + project + "/locations/" + loc
		members := map[string][]string{}
		rit := c.ListReservations(ctx, &reservationpb.ListReservationsRequest{Parent: parent})
		for rv, err := rit.Next(); err == nil; rv, err = rit.Next() {
			if g := rv.GetReservationGroup(); g != "" {
				members[g] = append(members[g], lastSegment(rv.GetName()))
			}
		}
		it := c.ListReservationGroups(ctx, &reservationpb.ListReservationGroupsRequest{Parent: parent})
		for {
			g, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return console.Listing{}, errors.New(consoleErr(err))
			}
			listing.Items = append(listing.Items, console.Resource{Name: loc + "/" + lastSegment(g.GetName()), Fields: map[string]string{
				"Location": loc, "Reservations": orDash(strings.Join(members[g.GetName()], ", "))}})
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p bqReservationGroupsProvider) CreateForm() (string, []console.Field) {
	return "Create reservation group", []console.Field{
		{Name: "id", Label: "Reservation group name", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,62}[a-z0-9])?$`},
		{Name: "location", Label: "Location", Type: "select", Required: true, Options: capacityLocations, Default: "US"},
	}
}

func (p bqReservationGroupsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return "", err
	}
	defer c.Close()
	g, err := c.CreateReservationGroup(ctx, &reservationpb.CreateReservationGroupRequest{
		Parent:             "projects/" + bqCapacityProvider(p).project(project) + "/locations/" + v["location"],
		ReservationGroupId: v["id"], ReservationGroup: &reservationpb.ReservationGroup{}})
	if err != nil {
		return "", errors.New(consoleErr(err))
	}
	return v["location"] + "/" + lastSegment(g.GetName()), nil
}

func (p bqReservationGroupsProvider) Delete(ctx context.Context, project, row string) error {
	loc, id, ok := strings.Cut(row, "/")
	if !ok {
		return fmt.Errorf("%q is not a reservation group", row)
	}
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	name := "projects/" + bqCapacityProvider(p).project(project) + "/locations/" + loc + "/reservationGroups/" + id
	if err := c.DeleteReservationGroup(ctx, &reservationpb.DeleteReservationGroupRequest{Name: name}); err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

// BI Engine: BigQuery's BI Engine page, a location's BI reservation and
// its size ("Create reservation", "Edit", "Delete" of
// https://docs.cloud.google.com/bigquery/docs/bi-engine-reserve-capacity,
// read 2026-11-23), each updateBiReservation; Delete sets its size to 0.

type bqBIEngineProvider struct{ bq bigqueryProvider }

func (bqBIEngineProvider) ID() string    { return "bigquery-bi-engine" }
func (bqBIEngineProvider) Title() string { return "BI Engine" }

const gib = int64(1) << 30

func (p bqBIEngineProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Size (GiB)", "Preferred tables", "Last updated"}, NameColumn: "Location", Noun: "BI Engine reservations"}
	project = bqCapacityProvider(p).project(project)
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()
	for _, loc := range capacityLocations {
		bi, err := c.GetBiReservation(ctx, &reservationpb.GetBiReservationRequest{Name: "projects/" + project + "/locations/" + loc + "/biReservation"})
		if err != nil {
			return console.Listing{}, errors.New(consoleErr(err))
		}
		if bi.GetSize() == 0 {
			continue
		}
		var tables []string
		for _, t := range bi.GetPreferredTables() {
			tables = append(tables, t.GetDatasetId()+"."+t.GetTableId())
		}
		listing.Items = append(listing.Items, console.Resource{Name: loc, Fields: map[string]string{
			"Location": loc, "Size (GiB)": strconv.FormatFloat(float64(bi.GetSize())/float64(gib), 'f', -1, 64),
			"Preferred tables": orDash(strings.Join(tables, ", ")),
			"Last updated":     bi.GetUpdateTime().AsTime().UTC().Format("Jan 2, 2006, 3:04:05 PM UTC")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p bqBIEngineProvider) CreateForm() (string, []console.Field) {
	return "Create reservation", []console.Field{
		{Name: "location", Label: "Location", Type: "select", Required: true, Options: capacityLocations, Default: "US"},
		{Name: "size", Label: "GiB of capacity", Type: "number", Required: true, Default: "1", Help: "At most 250 GiB a project and location."},
		{Name: "tables", Label: "Preferred tables", Type: "text", Help: "dataset.table, separated by commas; empty for every table."},
	}
}

func (p bqBIEngineProvider) setBI(ctx context.Context, project, loc string, size int64, tables string) error {
	c, err := reservationAPI(ctx, p.bq.endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	project = bqCapacityProvider(p).project(project)
	bi := &reservationpb.BiReservation{Name: "projects/" + project + "/locations/" + loc + "/biReservation", Size: size}
	for _, t := range strings.Split(tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			ds, tb, ok := strings.Cut(t, ".")
			if !ok {
				return fmt.Errorf("preferred table %q is not dataset.table", t)
			}
			bi.PreferredTables = append(bi.PreferredTables, &reservationpb.TableReference{ProjectId: project, DatasetId: ds, TableId: tb})
		}
	}
	_, err = c.UpdateBiReservation(ctx, &reservationpb.UpdateBiReservationRequest{BiReservation: bi,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size", "preferred_tables"}}})
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

func (p bqBIEngineProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(v["size"]), 64)
	if err != nil || n <= 0 || n > 250 {
		return "", errors.New("GiB of capacity: a number above 0, at most 250")
	}
	if err := p.setBI(ctx, project, v["location"], int64(n*float64(gib)), v["tables"]); err != nil {
		return "", err
	}
	return v["location"], nil
}

func (p bqBIEngineProvider) Delete(ctx context.Context, project, loc string) error {
	return p.setBI(ctx, project, loc, 0, "")
}

var (
	_ console.PolicyEditor = bqCapacityProvider{}
	_ console.PathActor    = bqCommitmentsProvider{}
	_ console.Creator      = bqReservationGroupsProvider{}
	_ console.Deleter      = bqReservationGroupsProvider{}
	_ console.Creator      = bqBIEngineProvider{}
	_ console.Deleter      = bqBIEngineProvider{}
)
