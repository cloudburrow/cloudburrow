// Package analyticshub implements BigQuery sharing, formerly Analytics Hub
// (google.cloud.bigquery.analyticshub.v1, analyticshub.googleapis.com,
// #1960): data exchanges, listings of BigQuery datasets, subscriptions and
// their IAM policies.
//
// Google publishes no emulator for it, so it is built. Subscribing to a
// listing makes a linked dataset in the subscriber's project on the
// instance's BigQuery, through the Linker the CLI wires: "a read-only
// BigQuery dataset that serves as a pointer or reference to a shared
// dataset" (https://docs.cloud.google.com/bigquery/docs/analytics-hub-introduction).
// The BigQuery front keeps it a pointer (bigqueryfront/hublinks.go): its
// tables are views of the shared dataset's, kept in step with it on every
// read, every write into it is refused, and a listing's restricted export
// configuration is enforced on it there.
package analyticshub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// LinkedDataset is a subscription's linked dataset, as the Linker makes it
// in BigQuery.
type LinkedDataset struct {
	// Project and Dataset name the linked dataset, in the subscriber's
	// project.
	Project, Dataset string
	Location         string
	FriendlyName     string
	Description      string
	Labels           map[string]string
	// SourceProject and SourceDataset are the listing's shared dataset.
	SourceProject, SourceDataset string
	// Tables are the listing's selected tables, or none for every table.
	Tables []string
	// Restricted is restrictedExportConfig.enabled, RestrictQueryResult
	// its restrict_query_result; Revoked a revoked subscription's, whose
	// linked dataset can no longer be queried.
	Restricted, RestrictQueryResult, Revoked bool
	// Resources are a data clean room subscription's (#2101): the one
	// linked dataset holds every listing's shared table, view or routine,
	// each from its own dataset. Empty for a listing's subscription.
	Resources []LinkedResource `json:",omitempty"`
	// CleanRoom marks a data clean room's linked dataset, whose tables
	// and routines are Resources.
	CleanRoom bool `json:",omitempty"`
}

// LinkedResource is one shared resource of a clean room's linked
// dataset: a table or view (Table), or a table-valued function (Routine),
// of SourceProject.SourceDataset.
type LinkedResource struct {
	SourceProject, SourceDataset string
	Table, Routine               string `json:",omitempty"`
	// RestrictQueryResult is its listing's restrict_query_result.
	RestrictQueryResult bool `json:",omitempty"`
}

// Linker makes and changes linked datasets in the instance's BigQuery.
type Linker interface {
	// DatasetLocation is a dataset's location, or a NOT_FOUND error.
	DatasetLocation(ctx context.Context, project, dataset string) (string, error)
	// CreateLinked makes a linked dataset; ALREADY_EXISTS when the
	// subscriber's project has a dataset of its ID.
	CreateLinked(ctx context.Context, l LinkedDataset) error
	// UpdateLinked changes what the front enforces on one.
	UpdateLinked(ctx context.Context, l LinkedDataset) error
	// DeleteLinked deletes one; a dataset already gone is not an error.
	DeleteLinked(ctx context.Context, project, dataset string) error
}

type exchangeRecord struct {
	Exchange *ahpb.DataExchange
	Policy   *iampolicy.Stored
}

type listingRecord struct {
	Listing *ahpb.Listing
	Policy  *iampolicy.Stored
}

type subscriptionRecord struct {
	Sub    *ahpb.Subscription
	Policy *iampolicy.Stored
	// Linked is the linked dataset, kept after a revoke, when the
	// subscription's own linked maps are empty.
	Linked LinkedDataset
	// Deleted is when DeleteSubscription removed it, for
	// ListSharedResourceSubscriptions' include_deleted_subscriptions.
	Deleted time.Time
}

// Server serves google.cloud.bigquery.analyticshub.v1.AnalyticsHubService.
type Server struct {
	ahpb.UnimplementedAnalyticsHubServiceServer

	link  Linker
	now   func() time.Time
	state string

	mu        sync.Mutex
	exchanges map[string]*exchangeRecord
	listings  map[string]*listingRecord
	subs      map[string]*subscriptionRecord
	templates map[string]*ahpb.QueryTemplate
	ops       map[string]*longrunningpb.Operation
	// orgOf names a project's organization, for ListOrgDataExchanges.
	orgOf func(project string) string
}

// NewServer returns the service, its records kept in the file state (""
// keeps them in memory). link may be nil: a subscription is then refused.
func NewServer(state string, link Linker, now func() time.Time) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	s := &Server{link: link, now: now, state: state}
	s.clear()
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// OrganizationOf sets how a project's organization ("organizations/N", or
// "") is found, for ListOrgDataExchanges.
func (s *Server) OrganizationOf(f func(project string) string) {
	s.mu.Lock()
	s.orgOf = f
	s.mu.Unlock()
}

func (s *Server) clear() {
	s.exchanges = map[string]*exchangeRecord{}
	s.listings = map[string]*listingRecord{}
	s.subs = map[string]*subscriptionRecord{}
	s.templates = map[string]*ahpb.QueryTemplate{}
	s.ops = map[string]*longrunningpb.Operation{}
}

// Register adds the service and its operations to a gRPC server or to the
// JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	ahpb.RegisterAnalyticsHubServiceServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
}

// Reset forgets every record of project, or of every project when project
// is "". Linked datasets are BigQuery's, which its own reset clears.
func (s *Server) Reset(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.clear()
		return s.save()
	}
	prefix := "projects/" + project + "/"
	for k := range s.exchanges {
		if strings.HasPrefix(k, prefix) {
			delete(s.exchanges, k)
		}
	}
	for k := range s.listings {
		if strings.HasPrefix(k, prefix) {
			delete(s.listings, k)
		}
	}
	for k := range s.subs {
		if strings.HasPrefix(k, prefix) {
			delete(s.subs, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
		}
	}
	for k := range s.templates {
		if strings.HasPrefix(k, prefix) {
			delete(s.templates, k)
		}
	}
	return s.save()
}

// ---- persistence ----

type disk struct {
	Exchanges map[string]json.RawMessage `json:"exchanges"`
	Listings  map[string]json.RawMessage `json:"listings"`
	Subs      map[string]diskSub         `json:"subscriptions"`
	Policies  map[string]json.RawMessage `json:"policies"`
	Ops       map[string]json.RawMessage `json:"operations"`
	Templates map[string]json.RawMessage `json:"queryTemplates,omitempty"`
}

type diskSub struct {
	Sub     json.RawMessage `json:"subscription"`
	Linked  LinkedDataset   `json:"linked"`
	Deleted time.Time       `json:"deleted,omitzero"`
}

func (s *Server) load() error {
	if s.state == "" {
		return nil
	}
	b, err := os.ReadFile(s.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Analytics Hub state: %w", err)
	}
	var d disk
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("parse %s: %w", s.state, err)
	}
	policy := func(name string) (*iampolicy.Stored, error) {
		raw, ok := d.Policies[name]
		if !ok {
			return nil, nil
		}
		p := &iampolicy.Stored{}
		return p, json.Unmarshal(raw, p)
	}
	for n, raw := range d.Exchanges {
		e := &ahpb.DataExchange{}
		if err := protojson.Unmarshal(raw, e); err != nil {
			return fmt.Errorf("parse data exchange %s: %w", n, err)
		}
		p, err := policy(n)
		if err != nil {
			return err
		}
		s.exchanges[n] = &exchangeRecord{Exchange: e, Policy: p}
	}
	for n, raw := range d.Listings {
		l := &ahpb.Listing{}
		if err := protojson.Unmarshal(raw, l); err != nil {
			return fmt.Errorf("parse listing %s: %w", n, err)
		}
		p, err := policy(n)
		if err != nil {
			return err
		}
		s.listings[n] = &listingRecord{Listing: l, Policy: p}
	}
	for n, ds := range d.Subs {
		sub := &ahpb.Subscription{}
		if err := protojson.Unmarshal(ds.Sub, sub); err != nil {
			return fmt.Errorf("parse subscription %s: %w", n, err)
		}
		p, err := policy(n)
		if err != nil {
			return err
		}
		s.subs[n] = &subscriptionRecord{Sub: sub, Policy: p, Linked: ds.Linked, Deleted: ds.Deleted}
	}
	for n, raw := range d.Ops {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("parse operation %s: %w", n, err)
		}
		s.ops[n] = op
	}
	for n, raw := range d.Templates {
		t := &ahpb.QueryTemplate{}
		if err := protojson.Unmarshal(raw, t); err != nil {
			return fmt.Errorf("parse query template %s: %w", n, err)
		}
		s.templates[n] = t
	}
	return nil
}

// save writes every record; the caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	d := disk{Exchanges: map[string]json.RawMessage{}, Listings: map[string]json.RawMessage{}, Subs: map[string]diskSub{},
		Policies: map[string]json.RawMessage{}, Ops: map[string]json.RawMessage{}, Templates: map[string]json.RawMessage{}}
	keep := func(name string, p *iampolicy.Stored) error {
		if p == nil {
			return nil
		}
		b, err := json.Marshal(p)
		d.Policies[name] = b
		return err
	}
	for n, e := range s.exchanges {
		b, err := protojson.Marshal(e.Exchange)
		if err != nil {
			return err
		}
		d.Exchanges[n] = b
		if err := keep(n, e.Policy); err != nil {
			return err
		}
	}
	for n, l := range s.listings {
		b, err := protojson.Marshal(l.Listing)
		if err != nil {
			return err
		}
		d.Listings[n] = b
		if err := keep(n, l.Policy); err != nil {
			return err
		}
	}
	for n, r := range s.subs {
		b, err := protojson.Marshal(r.Sub)
		if err != nil {
			return err
		}
		d.Subs[n] = diskSub{Sub: b, Linked: r.Linked, Deleted: r.Deleted}
		if err := keep(n, r.Policy); err != nil {
			return err
		}
	}
	for n, op := range s.ops {
		b, err := protojson.Marshal(op)
		if err != nil {
			return err
		}
		d.Ops[n] = b
	}
	for n, t := range s.templates {
		b, err := protojson.Marshal(t)
		if err != nil {
			return err
		}
		d.Templates[n] = b
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// ---- names and checks ----

var (
	locationRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	exchangeRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/dataExchanges/([^/]+)$`)
	listingRE      = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/dataExchanges/([^/]+)/listings/([^/]+)$`)
	subscriptionRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/subscriptions/([^/]+)$`)
	orgLocationRE  = regexp.MustCompile(`^organizations/([^/]+)/locations/([^/]+)$`)
	// datasetRE is a listing's bigquery_dataset.dataset.
	datasetRE = regexp.MustCompile(`^projects/([^/]+)/datasets/([^/]+)$`)
	// tableRE is a selected resource's table.
	tableRE = regexp.MustCompile(`^projects/([^/]+)/datasets/([^/]+)/tables/([^/]+)$`)
	// routineRE is a clean room listing's selected routine (#2101).
	routineRE = regexp.MustCompile(`^projects/([^/]+)/datasets/([^/]+)/routines/([^/]+)$`)
	// queryTemplateRE is a clean room's query template (#2101).
	queryTemplateRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/dataExchanges/([^/]+)/queryTemplates/([^/]+)$`)
	// idRE is a data exchange or listing ID: "Unicode letters, numbers
	// (0-9), underscores (_). Max length: 100 bytes."
	idRE = regexp.MustCompile(`^[\p{L}\p{N}_]+$`)
	// displayNameRE: "Unicode letters, numbers (0-9), underscores (_),
	// dashes (-), spaces ( ), ampersands (&) and must not start or end with
	// spaces."
	displayNameRE = regexp.MustCompile(`^[\p{L}\p{N}_\-&]([\p{L}\p{N}_\- &]*[\p{L}\p{N}_\-&])?$`)
	// datasetIDRE is a BigQuery dataset ID: letters, numbers and
	// underscores, up to 1,024 characters.
	datasetIDRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

func checkID(field, id string) error {
	if id == "" {
		return apierror.InvalidArgument("%s is required", field)
	}
	if len(id) > 100 || !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s %q must contain only Unicode letters, numbers (0-9) and underscores (_), at most 100 bytes", field, id)
	}
	return nil
}

func checkDisplayName(name string) error {
	if name == "" {
		return apierror.InvalidArgument("display_name is required")
	}
	if len(name) > 63 || !displayNameRE.MatchString(name) {
		return apierror.InvalidArgument("display_name %q must contain only Unicode letters, numbers (0-9), underscores (_), dashes (-), spaces ( ) and ampersands (&), must not start or end with spaces, and is at most 63 bytes", name)
	}
	return nil
}

// checkText checks a description's control characters and a field's
// length in bytes.
func checkText(field, s string, max int) error {
	if len(s) > max {
		return apierror.InvalidArgument("%s is %d bytes; at most %d are allowed", field, len(s), max)
	}
	if !utf8.ValidString(s) {
		return apierror.InvalidArgument("%s is not valid UTF-8", field)
	}
	if field == "description" {
		for _, r := range s {
			if (r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f') || (r >= 0x7f && r <= 0x9f) || r == 0xfffe || r == 0xffff {
				return apierror.InvalidArgument("description must not contain C0 or C1 control codes other than tabs, new lines, carriage returns and page breaks")
			}
		}
	}
	return nil
}

// maxIcon is "Max Size: 3.0MiB" of the encoded data.
const maxIcon = 3 << 20

func checkIcon(icon []byte) error {
	if (len(icon)+2)/3*4 > maxIcon {
		return apierror.InvalidArgument("icon is larger than 3.0MiB encoded")
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clone[M proto.Message](m M) M { return proto.Clone(m).(M) }

func sortedKeys[V any](m map[string]V, prefix string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
