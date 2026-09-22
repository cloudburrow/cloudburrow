// Package console serves CloudBurrow's local web console.
//
// The console is a **view**, not a second system. Every resource it shows is
// read through the same surfaces an SDK client uses, so a bucket created with
// the Go client appears here and one created here is visible to the client.
// Keeping a store of its own would give CloudBurrow two answers to the same
// question, and the one the developer saw would be whichever they happened to
// ask.
//
// The backend layer here is deliberately thin: it exists because a browser
// cannot speak gRPC or hold cluster credentials, not to add behaviour. Nothing
// in it decides anything a service would decide differently.
package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed assets
var assets embed.FS

// Resource is one item on a list screen.
//
// It is deliberately generic: the console renders tables, and a per-service
// shape for each would multiply the backend without changing what a table
// draws. Service-specific detail lives in Fields.
type Resource struct {
	// Name is the resource's own name, as its API reports it.
	Name string `json:"name"`
	// Status is a short state word, or empty when the resource has no state.
	Status string `json:"status,omitempty"`
	// Fields are additional columns, rendered in the order Columns gives.
	Fields map[string]string `json:"fields,omitempty"`
	// Link is the detail route, or empty when there is no detail screen.
	Link string `json:"link,omitempty"`
	// Opens is the resource path this row leads to, when the listing's own
	// rule does not cover it.
	//
	// A bucket's Objects section mixes folders, which open, with objects,
	// which do not — so openability is per row there, while a listing whose
	// rows all open says so once with RowsOpenable.
	Opens []string `json:"opens,omitempty"`
	// Actions are the operations available on this resource.
	Actions []Action `json:"actions,omitempty"`
}

// Listing is a page of resources.
type Listing struct {
	// Columns names the Fields keys to render, in order.
	Columns []string `json:"columns"`
	// NameColumn labels the first column, which always carries Resource.Name.
	// Empty means "Name".
	//
	// It exists because a screen whose primary key is not called a name ended
	// up with two columns headed "Name": the implicit one and a field of the
	// same label. The screen decides what its identifier is called.
	NameColumn string `json:"nameColumn,omitempty"`
	// Noun is the plural word for these resources, used in the filter and the
	// empty state. Empty falls back to the screen's title, which reads badly
	// when the title is not a noun for the rows — "Filter resource manager".
	Noun  string     `json:"noun,omitempty"`
	Items []Resource `json:"items"`
	// Total is the number of resources before paging.
	Total int `json:"total"`
	// Unavailable, when set, means the service could not be reached. It is
	// distinct from an empty list: a developer shown an empty table for a
	// broken backend goes looking for a bug in their own code.
	Unavailable string `json:"unavailable,omitempty"`
	// Prompt means the screen needs something from the user before it can
	// show anything — a project, most often.
	//
	// It is deliberately not Unavailable. "Choose a project" is a
	// precondition, not a failure, and rendering it as a red error taught the
	// user that a working instance was broken. The distinction is the same one
	// the empty state makes: nothing here yet is not the same as cannot read.
	Prompt string `json:"prompt,omitempty"`
	// RowsOpenable declares that each row has a level below it, so the client
	// renders the name as a link into the next path segment.
	//
	// Declared rather than assumed: a listing whose rows open and one whose
	// rows do not look identical from the client's side, and guessing wrong
	// either hides a level or offers a link that 501s.
	RowsOpenable bool `json:"rowsOpenable,omitempty"`
	// AlwaysStatus declares that this listing has a status column even when no
	// row currently carries one.
	//
	// The client used to infer the column from the rows it happened to
	// receive, so a column appeared and disappeared as the data changed — and
	// a sort applied to it was silently lost on the next refresh. Whether a
	// listing has a status is a property of the listing, which only the
	// provider knows.
	AlwaysStatus bool `json:"alwaysStatus,omitempty"`
	// Note carries a caveat about what the listing actually shows — for
	// instance that a filter the screen offers is not honoured by the
	// backend. A screen that silently ignores a filter is lying about what
	// the rows are.
	Note string `json:"note,omitempty"`
	// Cursor is the token that fetches the rows after these.
	//
	// Every content listing stopped at a fixed limit and said "there may be
	// more", which is a wall with a label on it: the two-hundred-and-first row
	// was unreachable from the console at all. A provider that can continue a
	// read returns the token to continue it with; one that cannot leaves this
	// empty and keeps saying so, which is at least honest.
	//
	// Opaque to the client, and meaningful only to the provider that issued it:
	// a row offset for SQL, a document id for Firestore, a datastore cursor, a
	// row key for Bigtable.
	Cursor string `json:"cursor,omitempty"`
	// More reports that the cursor will return something. Separate from Cursor
	// being non-empty because a provider can only know it has reached the end by
	// asking for one row more than it shows, and a client should not have to
	// fetch a page to find out it is empty.
	More bool `json:"more,omitempty"`
}

// Provider reads live state for one service.
//
// Implementations call the service's own API rather than reaching into a
// store, which is what keeps the console a view.
type Provider interface {
	// ID is the URL segment and navigation key, e.g. "storage".
	ID() string
	// Title is the screen name, matching the console page it mirrors.
	Title() string
	// List returns the resources in a project.
	List(ctx context.Context, project string) (Listing, error)
}

// Field describes one input on a create form.
//
// The form is described by the backend rather than hard-coded in the client
// so that a field only ever appears when the service behind it can actually
// accept it — a form offering something the API refuses is the working-looking
// control the parity specification forbids.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
	// Pattern is the constraint the API itself enforces, so the form refuses
	// what the API would refuse rather than letting a round trip do it.
	Pattern string `json:"pattern,omitempty"`
	Default string `json:"default,omitempty"`
	// Section groups the field under a heading. A form whose fields carry no
	// section renders as one ungrouped block, which is what a two-field form
	// should look like.
	Section string `json:"section,omitempty"`
	// Immutable marks a field shown for context and refused on submit. An
	// edit form that hides a resource's identity makes the operator guess
	// which resource they are editing; one that accepts a change to it lies,
	// because the API will not apply it.
	Immutable bool `json:"immutable,omitempty"`
}

// ParseMap decodes a "map" field's value.
//
// Labels, annotations and environment variables are the fields a console
// cannot express as a scalar, and every one of them is a string-to-string
// map. Rather than widen the submitted value type — which would change every
// Creator in the tree so that one field type could be non-scalar — a map
// field carries a JSON object in its string, and this is the one place that
// knows it. A provider calls this instead of writing its own decode, so the
// encoding has a single definition.
//
// An empty value is an empty map and not an error: a form submitted without
// touching the labels field means "no labels", not "malformed".
func ParseMap(value string) (map[string]string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return map[string]string{}, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("expected a JSON object of string keys and values: %w", err)
	}
	return out, nil
}

// FormatMap encodes a map for a "map" field's prefilled value.
func FormatMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		// json.Marshal of a map[string]string cannot fail; returning an empty
		// value rather than panicking keeps a form bug out of the serving
		// path.
		return ""
	}
	return string(encoded)
}

// Creator is a provider whose resources can be created from the console.
//
// A provider that does not implement it gets no create button, which is how
// an unsupported operation stays absent rather than disabled-and-mysterious.
type Creator interface {
	// CreateForm describes the form, and its submit label. The label matches
	// the console's own wording — "Create", "Create topic", "Create queue".
	CreateForm() (label string, fields []Field)
	// Create makes the resource and returns its name.
	Create(ctx context.Context, project string, values map[string]string) (string, error)
}

// PageCreator is a Creator whose form belongs on a page of its own rather
// than in a dialog.
//
// A form with more than three fields is routed to a page without asking. This
// interface is how a shorter form that still needs the room says so: a
// multi-line value, or fields grouped under headings with the explanatory
// text a 440px dialog has nowhere to put.
type PageCreator interface {
	Creator
	// CreateOnPage reports whether the form gets its own page.
	CreateOnPage() bool
}

// Pager is a provider whose listings can be continued past their first page.
//
// Separate from Provider and Driller because paging is a property of a specific
// listing, not of a product: a Cloud SQL table list pages and its Activity tab
// does not, since pg_stat_activity is a snapshot and an offset into a snapshot
// is meaningless.
type Pager interface {
	// Page returns the rows after a cursor, for the resource at a path. An empty
	// path means the list screen itself.
	//
	// The cursor is one this provider issued. A cursor it does not recognise is
	// an error, not an empty page: silently returning nothing would look
	// identical to reaching the end.
	Page(ctx context.Context, project string, path []string, cursor string) (Listing, error)
}

// Driller is a provider whose resources contain something worth opening.
//
// A list of collections answers "what is there"; it does not answer "did my
// application write what I expected", which is the question someone actually
// opens a database console to settle. Detail returns the level below one row —
// the documents in a collection, the rows in a table — as an ordinary listing,
// so the same renderer draws it and a screen cannot drift from the list it
// came from.
type Driller interface {
	// Detail returns the page for a resource, addressed by an ordered path.
	//
	// A path rather than a name because a resource contains resources: a
	// database holds a table which holds columns, a bucket holds a prefix
	// which holds objects. With a single leaf name the console could express
	// exactly two levels, so the third — the one a developer opens a database
	// console to reach — had nowhere to live.
	//
	// path[0] is the row on the list screen. A provider that understands only
	// that much says so for anything deeper rather than guessing, which is
	// what DeeperThan is for.
	Detail(ctx context.Context, project string, path []string) (Detail, error)
}

// DeeperThan reports a path this provider cannot open.
//
// Returned as an unavailable Detail rather than an error: the screen exists
// and says what it cannot show, which is the same distinction Listing.Prompt
// draws between a precondition and a failure.
func DeeperThan(level int, path []string) Detail {
	return Detail{Unavailable: fmt.Sprintf(
		"this resource has no level below %s", strings.Join(path[:level], "/"))}
}

// Detail is one resource's page: what it is, and what is inside it.
//
// It returns sections rather than a listing because a resource page with
// exactly one aspect is not a resource page — it answers "what is inside
// this" and loses "what is this", which is the question the screen's own
// title implies.
type Detail struct {
	// Summary is the resource's own properties, in order.
	//
	// Ordered rather than a map, because a map has no order and these are
	// read as a block. The provider supplies them rather than the client
	// remembering the row it was clicked from, so a deep link shows the same
	// page as a click-through.
	Summary []Property `json:"summary,omitempty"`
	// Sections are the page's aspects, in tab order. A provider offering one
	// renders no tab strip: a strip of one is a control that does nothing.
	Sections []Section `json:"sections"`
	// Unavailable and Prompt carry the same meanings they do on a Listing,
	// for the cases where the resource itself cannot be reached at all. A
	// section that individually fails carries its own.
	Unavailable string `json:"unavailable,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	// Actions are what can be done to this resource from its own page.
	//
	// A list row's actions come from Actor, which is addressed by name and so
	// can only ever reach the top level. A secret version, a table, a
	// subscription — everything the path was added for — had no way to offer
	// one, so "open it" and "do something to it" were mutually exclusive.
	Actions []Action `json:"actions,omitempty"`
	// Query is the query surface for this resource, when it differs from the
	// service's. A Bigtable table's column families are not a Firestore
	// collection's fields, so the form has to be built for the resource being
	// looked at rather than once for the product.
	//
	// Set by the server from the provider's QueryForm, so the form the page
	// draws is the form the query route will validate against.
	Query *QuerySpec `json:"query,omitempty"`
	// Reveal is the label for the control that shows this resource's own secret
	// value, or empty when it has none. Set by the server from the provider's
	// CanReveal, so a page cannot offer a reveal the route would refuse.
	Reveal string `json:"reveal,omitempty"`
	// Edit is the form this resource can be changed through, prefilled with
	// what it holds now. Nil means it cannot be edited, which is why the
	// button is absent rather than present and refusing.
	Edit *EditForm `json:"edit,omitempty"`
}

// EditForm is the form a resource is changed through.
type EditForm struct {
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
	// Note names what cannot be changed, and why. An edit form that silently
	// omits the immutable half reads as though everything absent from it does
	// not exist.
	Note string `json:"note,omitempty"`
}

// Property is one label/value pair on a resource's summary.
type Property struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// SectionKind says what a section actually holds.
//
// A section could only ever be a table, so everything a resource page needs
// to say that is not rows — its configuration, its YAML, a chart — had
// nowhere to go, and "open this resource" meant "here is one more list".
// Inferring the kind from which field happened to be populated would put the
// decision in the client; the provider knows and says.
type SectionKind string

const (
	// KindListing is rows. The default, so every provider written before this
	// existed keeps working unchanged.
	KindListing SectionKind = ""
	// KindProperties is label/value pairs, optionally under headings.
	KindProperties SectionKind = "properties"
	// KindText is preformatted text: a YAML document, a DDL statement.
	KindText SectionKind = "text"
	// KindChart is a series over time.
	KindChart SectionKind = "chart"
)

// Section is one aspect of a resource, rendered as a tab.
//
// Exactly one content field is populated, chosen by Kind. A section that
// carries two is a provider bug; the client draws the one Kind names and
// ignores the rest, which keeps a mistake visible as a wrong tab rather than
// as two overlapping renderings.
type Section struct {
	// ID is what goes in the URL, so a tab is linkable and survives a reload.
	ID string `json:"id"`
	// Label is the tab's text.
	Label string `json:"label"`
	// Kind selects the content field below. Empty means Listing, so this is
	// backward compatible by construction.
	Kind SectionKind `json:"kind,omitempty"`
	// Listing is the section's content when Kind is KindListing, drawn by the
	// same table renderer as every other listing.
	Listing Listing `json:"listing,omitempty"`
	// Groups are the content when Kind is KindProperties. Headed groups
	// rather than a flat map, because a resource's configuration divides —
	// "Networking" and "Security" are not one list of pairs.
	Groups []PropertyGroup `json:"groups,omitempty"`
	// Text is the content when Kind is KindText. Newlines are significant and
	// the client renders it monospace.
	Text string `json:"text,omitempty"`
	// Series is the content when Kind is KindChart.
	Series []ChartSeries `json:"series,omitempty"`
	// Unavailable explains why this one section has no content, so a section
	// that cannot be read shows its own error inside its own panel rather
	// than an empty table claiming the resource holds nothing.
	Unavailable string `json:"unavailable,omitempty"`
	// Note carries a caveat about what this section shows — a truncation, or
	// a value the backend does not honour.
	Note string `json:"note,omitempty"`
}

// PropertyGroup is a headed block of label/value pairs.
type PropertyGroup struct {
	Heading string `json:"heading,omitempty"`
	// Properties are the pairs, in the order the provider gives them.
	Properties []Property `json:"properties"`
}

// ChartSeries is one line on a chart, with its own points and unit.
//
// A point carrying a nil Value is a gap: a reading that was not taken. It is
// drawn as a break in the line rather than joined to its neighbours, because
// a straight line across a period nobody measured is the chart inventing the
// thing it exists to report.
type ChartSeries struct {
	Label string `json:"label"`
	// Unit is what the numbers are, shown on the chart: "cores", "bytes".
	Unit string `json:"unit,omitempty"`
	// Max is a known ceiling — a capacity — which makes the chart say how
	// much headroom there is rather than only how the value moved. Zero means
	// scale to the data.
	Max    float64      `json:"max,omitempty"`
	Points []ChartPoint `json:"points"`
}

// ChartPoint is one reading. Value is nil where no reading was taken.
type ChartPoint struct {
	At    string   `json:"at"`
	Value *float64 `json:"value"`
}

// Executor is a provider that can run a statement the user wrote.
//
// Every other interface here takes fixed input: a form's named fields, an
// action id from a list, a resource path. None carries user-authored text,
// so five products that already execute arbitrary SQL against a live backend
// had no way to be asked a question.
//
// The result is a Listing, which means the table renderer draws it with no
// new code and sorting, filtering and paging come for free.
type Executor interface {
	// Query runs statement against the resource named by path and returns the
	// result set.
	//
	// The backend's own error text is the answer when it fails — a syntax
	// error names the character, and a generic "query failed" throws away the
	// only useful part.
	Query(ctx context.Context, project string, path []string, statement string) (Listing, error)
	// QueryHint describes what this provider will accept, shown above the
	// editor so the refusal is visible before the statement is written
	// rather than after.
	QueryHint() string
}

// Builder is a provider whose query surface is a form rather than free text.
//
// Firestore, Datastore and Bigtable have no query language a console can offer.
// Their queries are structures — a field, an operator and a value; a row-key
// range — and the real console builds them with controls for exactly that
// reason. A textarea would mean inventing a syntax, and a syntax nobody else
// accepts is worse than no query at all.
type Builder interface {
	// QueryForm describes the controls for the resource at a path, and the
	// label on the button that runs them. No fields means this resource cannot
	// be queried, which is different from the service having no query surface.
	QueryForm(path []string) (label string, fields []Field)
	// Build runs the query the form describes and returns the rows.
	Build(ctx context.Context, project string, path []string, values map[string]string) (Listing, error)
}

// OptionalDriller is a Driller that cannot always open its rows.
//
// A Go interface is satisfied by a type, not by an instance, so one provider
// type shared by several screens advertises Detail for all of them the moment
// any one of them can serve it. That shipped: every kubeProvider screen
// reported detail:true while only Pods had a detail function, so clicking a
// Service, a Job or an Event produced a link that led to
// "Kubernetes Services rows cannot be opened" — the working-looking control
// AGENTS.md forbids, offered by the capability advertisement itself.
type OptionalDriller interface {
	Driller
	// CanDrill reports whether this instance can open its rows.
	CanDrill() bool
}

// Deleter is a provider whose resources can be deleted from the console.
type Deleter interface {
	// Delete removes one resource by the name List reported.
	Delete(ctx context.Context, project, name string) error
}

// Actor is a provider with named per-resource actions, such as pausing a
// queue.
type Actor interface {
	// Actions returns the actions available on a resource, by id and label.
	// Returning none means the resource has no actions, not that the screen
	// should invent some.
	Actions(resource Resource) []Action
	// Act performs one.
	Act(ctx context.Context, project, name, action string) error
}

// Action is one named operation on a resource.
type Action struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Destructive marks an action that discards data, so the client can
	// confirm it and name what is about to be affected.
	Destructive bool `json:"destructive,omitempty"`
	// Fields are the inputs the action needs. An action with none is performed
	// on click; one with fields opens a form first.
	//
	// "Add a version" needs a payload, "update traffic" needs a percentage.
	// Without this an action could only ever be a verb with no object, so
	// every operation that takes a value had to be modelled as a create or
	// left out.
	Fields []Field `json:"fields,omitempty"`
}

// PathActor is a provider with actions on the resources inside a resource.
//
// Actor addresses a resource by the name List reported, which reaches exactly
// the top level. A secret version, a Cloud Run revision and a Pub/Sub
// subscription all live below it, and all three have operations that matter.
type PathActor interface {
	// DetailActions returns what can be done to the resource at a path.
	//
	// The server attaches these to the Detail it serves, so the page draws them
	// without a second round trip, and calls this again before performing one —
	// so a request cannot reach an operation the page would not have offered.
	//
	// It takes the project because what is available usually depends on the
	// resource's current state: enabling an enabled secret version and
	// destroying a destroyed one are both buttons that exist only to fail, and
	// deciding that needs a read.
	DetailActions(ctx context.Context, project string, path []string) []Action
	// ActAt performs one. The values are the action's own fields, empty for an
	// action that declared none.
	ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error
}

// Revealer is a provider that can return a resource's own secret value.
//
// Separate from Detail on purpose. A payload included in a detail response is
// on screen because the page was opened, which means it is in the browser's
// memory and in the network log of anyone who happened to be looking. This is a
// route of its own so that showing a secret is an action somebody took,
// recorded in the operations ledger like any other, and never a side effect of
// navigation.
type Revealer interface {
	// Reveal returns the value at a path. The label names what is being
	// returned, for the dialog that shows it.
	Reveal(ctx context.Context, project string, path []string) (label, value string, err error)
	// CanReveal reports whether the path has a value to show, so the button is
	// absent rather than present and refusing.
	CanReveal(path []string) bool
}

// Editor is a provider whose resources can be changed in place.
//
// Creation and deletion were the only mutations the console could express, so
// every setting a resource has — a queue's rate limits, a subscription's ack
// deadline, a bucket's lifecycle — was readable and then permanently fixed.
// A provider that does not implement this gets no edit button.
type Editor interface {
	// Edit applies changed values to the resource at a path. The values are
	// the ones the form declared; a provider validates them rather than
	// trusting the client to have done it, because the form is a convenience
	// and the API is the authority.
	Edit(ctx context.Context, project string, path []string, values map[string]string) error
}

// Status is what the dashboard reports about the instance.
type Status struct {
	Instance string `json:"instance"`
	// DefaultProject is the project the console selects when the URL names
	// none.
	//
	// Without it the picker opened on "All projects", and the three services
	// that list per project answered with an error apiece — so a fresh
	// console's first screen was a wall of red on an instance that was
	// working perfectly. A console always has a project selected; that is
	// what the picker is for.
	DefaultProject string            `json:"defaultProject,omitempty"`
	Ready          bool              `json:"ready"`
	State          string            `json:"state"`
	Cluster        string            `json:"cluster"`
	Kubernetes     string            `json:"kubernetes,omitempty"`
	Namespace      string            `json:"namespace"`
	Mode           string            `json:"mode"`
	Services       []ServiceStatus   `json:"services"`
	Endpoints      map[string]string `json:"endpoints"`
	Components     map[string]bool   `json:"components,omitempty"`
	// Identity is the service account the generated credentials present, and
	// where they live.
	//
	// The account menu said "cloudburrow-local", hard-coded in the HTML, while
	// the credentials actually written name the project — so on any instance whose
	// project was not the default the console displayed an identity no client
	// would ever present. The menu now shows what the instance issued.
	Identity *Identity `json:"identity,omitempty"`
	// Tunnels reports each forwarder's live state.
	//
	// The component map latched at startup, so a tunnel whose pod went away
	// still read as ready — the dashboard answered "did this ever work"
	// while looking like it answered "is this working".
	Tunnels []TunnelStatus `json:"tunnels,omitempty"`
}

// Identity is what the instance's generated credentials claim to be.
//
// Nothing here is a secret: the email and the file path are what a client would
// print from the ADC fixture, and the private key is never read by the console
// at all. Stating them is the point — a developer debugging an auth problem needs
// to know which identity their tooling picked up.
type Identity struct {
	// ServiceAccount is the email the ADC fixture presents.
	ServiceAccount string `json:"serviceAccount"`
	// Project is the project the fixture is scoped to.
	Project string `json:"project,omitempty"`
	// CredentialsPath is where the fixture was written, so a developer can point
	// tooling at it.
	CredentialsPath string `json:"credentialsPath,omitempty"`
	// Authenticates is false on every CloudBurrow instance and is sent anyway, so
	// the claim is data the screen reads rather than a sentence the screen
	// invents.
	Authenticates bool `json:"authenticates"`
}

// TunnelStatus is one port-forward, as it is right now.
type TunnelStatus struct {
	Name string `json:"name"`
	Host string `json:"host,omitempty"`
	// Running is the current state of the supervised process, not the value
	// it had when the instance started.
	Running bool `json:"running"`
	// Restarts counts re-establishments. Surviving a restart and never
	// noticing one are different states, and a backend that flaps should be
	// visible as flapping rather than merely survivable.
	Restarts int `json:"restarts"`
}

// ServiceStatus is one service's availability.
type ServiceStatus struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Enabled bool   `json:"enabled"`
	// Reason explains a disabled service, so a greyed-out entry is never
	// unexplained.
	Reason string `json:"reason,omitempty"`
}

// StatusSource supplies the instance status.
type StatusSource func(ctx context.Context) Status

// Server serves the console.
type Server struct {
	addr      string
	providers map[string]Provider
	order     []string
	status    StatusSource
	logs      *Recorder
	// playground is never nil; an unconfigured one reports that local AI is
	// off rather than making every call site check.
	playground *Playground
	metrics    MetricsSource
	// series is the retained history the charts draw. Nil means this
	// instance keeps none, which the endpoint says rather than returning an
	// empty array that looks like an idle cluster.
	series *Series

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// New returns a console server bound to addr.
func New(addr string, status StatusSource, providers ...Provider) *Server {
	s := &Server{
		addr: addr, providers: map[string]Provider{}, status: status,
		logs:       NewRecorder(DefaultLogLimit, nil),
		playground: &Playground{},
	}
	for _, p := range providers {
		if p == nil {
			continue
		}
		s.providers[p.ID()] = p
		s.order = append(s.order, p.ID())
	}
	return s
}

func (s *Server) Name() string { return "console" }

// Logs exposes the recorder, so the process can feed it what it observes.
func (s *Server) Logs() *Recorder { return s.logs }

// Addr returns the resolved address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL returns the address a developer opens, or "" before Start.
func (s *Server) URL() string {
	if addr := s.Addr(); addr != "" {
		return "http://" + addr
	}
	return ""
}

// Handler builds the routes. Exported so tests drive it without a listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/services", s.handleServices)
	mux.HandleFunc("GET /api/resources/{service}", s.handleResources)
	mux.HandleFunc("GET /api/detail/{service}", s.handleDetail)
	mux.HandleFunc("POST /api/resources/{service}", s.handleCreate)
	mux.HandleFunc("DELETE /api/resources/{service}", s.handleDelete)
	mux.HandleFunc("POST /api/actions/{service}", s.handleAction)
	mux.HandleFunc("PATCH /api/resources/{service}", s.handleEdit)
	mux.HandleFunc("POST /api/reveal/{service}", s.handleReveal)
	mux.HandleFunc("GET /api/page/{service}", s.handlePage)
	mux.HandleFunc("POST /api/query/{service}", s.handleQuery)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/operations", s.handleOperations)
	mux.HandleFunc("GET /api/metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/metrics/series", s.handleSeries)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/ai/playground", s.handlePlayground)
	mux.HandleFunc("POST /api/ai/playground", s.handlePlaygroundGenerate)
	mux.HandleFunc("GET /api/stream", s.handleStream)

	ui, err := fs.Sub(assets, "assets")
	if err != nil {
		// The assets are embedded at build time; a failure here means the
		// binary is malformed, and serving a console with no UI would be
		// worse than saying so.
		panic("console assets are missing from the binary: " + err.Error())
	}
	files := http.FileServer(http.FS(ui))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Every non-asset path serves the shell, so a deep link opened
		// directly renders rather than 404ing. The client router then reads
		// the path.
		if isAsset(r.URL.Path) {
			files.ServeHTTP(w, r)
			return
		}
		s.serveShell(w, r)
	})

	return sameOriginOnly(noStore(mux))
}

func isAsset(path string) bool {
	for _, ext := range []string{".css", ".js", ".svg", ".png", ".ico", ".woff2", ".map"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func (s *Server) serveShell(w http.ResponseWriter, r *http.Request) {
	body, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "console assets are missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A console that loaded remote code would defeat the point of shipping
	// the assets: it would stop working offline and would widen what the page
	// can reach. The policy is enforced rather than merely intended.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; "+
			"form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(body)
}

// noStore keeps the browser from caching live state.
//
// A cached resource list is worse than a slow one: it shows a developer a
// bucket they deleted and lets them conclude the delete did not work.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginOnly rejects cross-site requests to the API.
//
// The console binds loopback, but loopback is reachable from any page the
// developer's browser has open. Without this, a visited website could drive
// the API — including deletes — because the browser would send the request
// happily. Sec-Fetch-Site is checked because it cannot be set by script.
func sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			// Empty covers clients that send no fetch metadata at all, such
			// as curl, which is not a browser and cannot be driven by a
			// visited page.
		default:
			http.Error(w, "cross-site requests to the console API are refused",
				http.StatusForbidden)
			return
		}
		// A cross-origin browser request also carries Origin; anything other
		// than our own is refused even if fetch metadata was stripped.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameHost(origin, r.Host) {
				http.Error(w, "cross-origin requests to the console API are refused",
					http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return trimmed == host
}

// readBudget bounds every read the console serves.
//
// A hung backend has to surface as an error the screen can show. An unbounded
// read leaves a skeleton shimmering with no elapsed time, no cancel and no
// eventual failure — the one state that fails invisibly, which is exactly what
// this console is not allowed to do.
const readBudget = 20 * time.Second

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.status == nil {
		writeJSON(w, http.StatusOK, Status{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	writeJSON(w, http.StatusOK, s.status(ctx))
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		p := s.providers[id]
		caps := s.capabilitiesOf(p)
		out = append(out, map[string]any{
			"id": p.ID(), "title": p.Title(),
			"create": caps.Create, "delete": caps.Delete, "detail": caps.Detail,
			"query": caps.Query, "edit": caps.Edit, "reveal": caps.Reveal,
			"page": caps.Page,
		})
	}
	// The playground is advertised only when local AI is configured, so the
	// navigation never offers a screen that cannot work. It is not a
	// provider — it has no listing — so it is appended rather than being
	// forced into the provider interface.
	if s.playground.Configured() {
		out = append(out, map[string]any{
			"id": "playground", "title": "AI Playground", "create": false, "delete": false,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("service")
	p, ok := s.providers[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no such service %q", id),
		})
		return
	}
	project := r.URL.Query().Get("project")

	// A bounded read: a hung backend must surface as an error the screen can
	// show, not as a request that never returns.
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()

	listing, err := p.List(ctx, project)
	if err != nil {
		// Reported as an unavailable listing rather than an HTTP error, so
		// the screen can render its error state with the cause instead of
		// showing an empty table. The collections are still empty arrays
		// rather than null, so a client that iterates before checking does
		// not fall over on top of the failure it was about to report.
		writeJSON(w, http.StatusOK, Listing{
			Unavailable: userMessage(err),
			Columns:     []string{}, Items: []Resource{},
		})
		return
	}
	if listing.Items == nil {
		listing.Items = []Resource{}
	}
	if listing.Columns == nil {
		listing.Columns = []string{}
	}
	// Per-resource actions are attached here rather than by the client, so
	// an action only appears when the provider actually offers it.
	if actor, ok := p.(Actor); ok {
		for i := range listing.Items {
			listing.Items[i].Actions = actor.Actions(listing.Items[i])
		}
	}
	writeJSON(w, http.StatusOK, listing)
}

// userMessage renders an error for a screen.
//
// A gRPC error stringifies as `rpc error: code = AlreadyExists desc = Topic
// already exists`, which puts the transport in front of the thing the
// developer needs to read. The code still matters — it says whether this is
// their mistake or ours — so it is kept and the envelope is dropped.
func userMessage(err error) string {
	if err == nil {
		return ""
	}
	// A deadline says nothing about what went wrong, so it is translated into
	// the only fact the user has: nothing came back. "context deadline
	// exceeded" on a screen sends a developer looking for a bug in their own
	// code. No duration is quoted here because the budgets differ by handler,
	// and one of them being wrong on screen is worse than neither being there.
	if errors.Is(err, context.DeadlineExceeded) {
		return "the local instance did not answer in time"
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.OK {
		if st.Code() == codes.DeadlineExceeded {
			return "the local instance did not answer in time"
		}
		return fmt.Sprintf("%s: %s", st.Code(), st.Message())
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Start binds the listener and serves. It does not block.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind console address %s: %w", s.addr, err)
	}

	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.srv, s.done = ln, srv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// Stop shuts the console down.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.srv = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}

// capabilities describes what a provider supports, so the client renders only
// controls that will work.
type capabilities struct {
	Create *createForm `json:"create,omitempty"`
	// Query means a statement can be run against this service's resources.
	Query  *queryCapability `json:"query,omitempty"`
	Delete bool             `json:"delete,omitempty"`
	// Detail means a row can be opened to show what is inside it.
	Detail bool `json:"detail,omitempty"`
	// Edit means a resource's detail page can offer an edit form. The form
	// itself comes from the resource, because what may be changed about a
	// queue is not what may be changed about a subscription.
	Edit bool `json:"edit,omitempty"`
	// Reveal means a resource can be asked for its own secret value. Whether a
	// particular resource has one is the resource's answer, not the service's.
	Reveal bool `json:"reveal,omitempty"`
	// Page means a listing that reports more rows can be continued. Whether a
	// particular listing can is said by that listing, with a cursor.
	Page bool `json:"page,omitempty"`
}

// QuerySpec is a query surface: either a statement box or a form.
//
// Exactly one of Hint and Fields is set. The client draws whichever it is given
// rather than deciding, because "does this database have a query language" is
// not a question the browser can answer.
type QuerySpec struct {
	Hint   string  `json:"hint,omitempty"`
	Fields []Field `json:"fields,omitempty"`
	Label  string  `json:"label,omitempty"`
}

// queryCapability describes a provider's query surface to the client.
//
// Exactly one of Hint and Fields is set. Hint means a statement box, Fields
// means a form; the client draws whichever it is given rather than deciding,
// because "does this database have a query language" is not a question the
// browser can answer.
type queryCapability = QuerySpec

type createForm struct {
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
	// Page means the form is rendered at its own address instead of in a
	// dialog. Decided here rather than in the client so that "long enough to
	// deserve a page" has one definition.
	Page bool `json:"page,omitempty"`
}

// createPageThreshold is the number of fields past which a create form stops
// being a dialog. Three is the largest form that fits the dialog without
// scrolling at the width the stylesheet gives it.
const createPageThreshold = 3

func (s *Server) capabilitiesOf(p Provider) capabilities {
	var c capabilities
	if creator, ok := p.(Creator); ok {
		label, fields := creator.CreateForm()
		c.Create = &createForm{Label: label, Fields: fields}
		c.Create.Page = len(fields) > createPageThreshold
		if pc, ok := p.(PageCreator); ok && pc.CreateOnPage() {
			c.Create.Page = true
		}
	}
	if e, ok := p.(Executor); ok {
		c.Query = &queryCapability{Hint: e.QueryHint()}
	}
	if b, ok := p.(Builder); ok {
		// The form is per resource, so the capability only says that one
		// exists; the detail page carries the fields for the resource being
		// looked at. A nil path asks the provider for its general shape, which
		// is what the list screen can advertise.
		label, fields := b.QueryForm(nil)
		if len(fields) > 0 {
			c.Query = &queryCapability{Label: label, Fields: fields}
		}
	}
	if _, ok := p.(Deleter); ok {
		c.Delete = true
	}
	// Asked of the instance when it can answer, because the interface alone
	// speaks for the type.
	if d, ok := p.(OptionalDriller); ok {
		c.Detail = d.CanDrill()
	} else if _, ok := p.(Driller); ok {
		c.Detail = true
	}
	if _, ok := p.(Editor); ok {
		c.Edit = true
	}
	if _, ok := p.(Revealer); ok {
		c.Reveal = true
	}
	if _, ok := p.(Pager); ok {
		c.Page = true
	}
	return c
}

// handleCreate creates a resource through the provider's own API.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	creator, ok := p.(Creator)
	if !ok {
		// Unimplemented rather than a generic error: the service exists and
		// creation is simply not offered for it.
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be created from the console",
		})
		return
	}

	var values map[string]string
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&values); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "malformed request: " + err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation("create", p.Title(), project)

	name, err := creator.Create(ctx, project, values)
	if err != nil {
		// The verdict comes from the backend, never from the console's own
		// optimism: an operation is not successful because a call returned.
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project,
			OperationID: opID, Message: "create failed: " + userMessage(err),
		})
		// The API's own message reaches the screen. A generic "could not
		// create" hides the constraint the caller actually violated. The
		// operation id goes with it: a failure is a record like any other,
		// and without the id the client cannot tell the server's copy of it
		// from its own and shows the failure twice.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.NameOperation(opID, name)
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project,
		Resource: name, OperationID: opID, Message: "created " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "operation": opID})
}

// handleDelete removes a resource.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	deleter, ok := p.(Deleter)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be deleted from the console",
		})
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name is required; refusing to delete without one",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation("delete", name, project)

	if err := deleter.Delete(ctx, project, name); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: "delete failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: "deleted " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name, "operation": opID})
}

// handleAction performs a named per-resource action.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	// The body is read before the provider is checked, because which interface
	// has to be satisfied depends on how the request addresses its target: a
	// row sends a name and needs Actor, a detail page sends a path and needs
	// PathActor. Checking Actor first refused every path-addressed action on a
	// provider that only implements the second.
	var req struct {
		Name, Action string
		// Path addresses a resource inside a resource. A row on the list
		// screen sends Name; a detail page sends Path, because a secret
		// version has no name the top-level list ever reported.
		Path []string
		// Values are the action's own fields, for an action that declared any.
		Values map[string]string
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Action == "" || (req.Name == "" && len(req.Path) == 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "action, and one of name or path, are required",
		})
		return
	}
	if len(req.Path) > 0 {
		s.actAtPath(w, r, p, req.Path, req.Action, req.Values)
		return
	}
	actor, ok := p.(Actor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " has no actions",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Instrumented exactly as create and delete are. An action is a mutation:
	// Cloud Tasks' purge destroys a queue's contents, and it was producing no
	// record in the operations ledger, in Activity or in the logs — so a
	// developer asking later where the tasks went found an empty history and
	// concluded nothing had happened.
	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation(req.Action, req.Name, project)

	if err := actor.Act(ctx, project, req.Name, req.Action); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: req.Name,
			OperationID: opID, Message: req.Action + " failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: req.Name,
		OperationID: opID, Message: req.Action + " applied to " + req.Name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"applied": req.Action, "operation": opID})
}

// actAtPath performs an action on a resource inside a resource.
//
// Split out rather than folded into handleAction so the two addressing modes
// stay visibly separate: a name-addressed action reaches Actor, a
// path-addressed one reaches PathActor, and neither silently falls through to
// the other.
func (s *Server) actAtPath(w http.ResponseWriter, r *http.Request, p Provider, path []string, action string, values map[string]string) {
	actor, ok := p.(PathActor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " has no actions on the resources inside a resource",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	project := r.URL.Query().Get("project")

	// The action must be one the page would have offered. Without this check
	// the route is a way to invoke any action name the provider happens to
	// understand on any path, which is wider than the UI it serves.
	offered := false
	for _, a := range actor.DetailActions(ctx, project, path) {
		if a.ID == action {
			offered = true
			break
		}
	}
	if !offered {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": action + " is not available on this resource",
		})
		return
	}

	// The whole path names the target in the ledger: the first segment alone
	// would say "enable demo-secret" when a version was enabled.
	name := strings.Join(path, "/")
	opID := s.logs.StartOperation(action, name, project)

	if err := actor.ActAt(ctx, project, path, action, values); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: action + " failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: action + " applied to " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"applied": action, "operation": opID})
}

// handlePage continues a listing past its first page.
//
// A GET, because it reads: a page of rows is addressable, cacheable and safe to
// retry, and a POST would say otherwise.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	pager, ok := p.(Pager)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be paged",
		})
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if cursor == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a cursor is required"})
		return
	}
	var path []string
	for _, segment := range r.URL.Query()["name"] {
		if segment = strings.TrimSpace(segment); segment != "" {
			path = append(path, segment)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()

	listing, err := pager.Page(ctx, r.URL.Query().Get("project"), path, cursor)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if listing.Items == nil {
		listing.Items = []Resource{}
	}
	if listing.Columns == nil {
		listing.Columns = []string{}
	}
	writeJSON(w, http.StatusOK, listing)
}

// handleReveal returns a resource's secret value, once, on request.
//
// Every reveal is an operation in the ledger with the path it named, because
// "who looked at this secret and when" is the question an audit asks. The value
// itself is never logged.
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	revealer, ok := p.(Revealer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " has no values to show",
		})
		return
	}

	var req struct{ Path []string }
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(req.Path) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	if !revealer.CanReveal(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this resource has no value to show",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	name := strings.Join(req.Path, "/")
	opID := s.logs.StartOperation("access", name, project)

	label, value, err := revealer.Reveal(ctx, project, req.Path)
	if err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	// The entry records the access, never the value. A log that carried the
	// payload would put the secret in the one place the console keeps history.
	s.logs.Log(Entry{
		Severity: SeverityWarning, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: "the value of " + name + " was shown in the console",
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"label": label, "value": value, "operation": opID,
	})
}

// handleEdit applies a change to one resource.
//
// PATCH rather than PUT: the form carries the fields the provider declared
// editable, not the whole resource, and a PUT would claim the absent fields
// were being cleared.
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	editor, ok := p.(Editor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be edited from the console",
		})
		return
	}

	var req struct {
		Path   []string
		Values map[string]string
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "malformed request: " + err.Error(),
		})
		return
	}
	if len(req.Path) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	name := strings.Join(req.Path, "/")
	opID := s.logs.StartOperation("update", name, project)

	if err := editor.Edit(ctx, project, req.Path, req.Values); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: "update failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: "updated " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"updated": name, "operation": opID})
}

// handleQuery runs a statement the user wrote.
//
// Shaped exactly like handleAction: the same 404, the same 501 for a provider
// that does not offer it, the same bounded body, the same operations ledger.
// A query is a mutation as far as the record is concerned — it is something
// the user did, and Activity should show it.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	var req struct {
		Path      []string
		Statement string
		// Values are a structured query's fields, for a provider with no query
		// language. A request carries one or the other, never both.
		Values map[string]string
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(req.Path) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the resource to query is required",
		})
		return
	}
	// Which interface the request needs depends on how it asks: a statement
	// reaches Executor, a set of values reaches Builder. Checking one before
	// reading the body would refuse every structured query against a provider
	// that has no query language — which is the whole set of providers Builder
	// exists for.
	statement := strings.TrimSpace(req.Statement)
	executor, canExecute := p.(Executor)
	builder, canBuild := p.(Builder)
	if !canExecute && !canBuild {
		// Unimplemented rather than a generic error: the service exists and
		// querying is simply not offered for it.
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be queried from the console",
		})
		return
	}
	switch {
	case statement != "" && !canExecute:
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " takes a query form, not a statement",
		})
		return
	case statement == "" && !canBuild:
		// A provider that only accepts statements was sent none. That is a bad
		// request, not a missing capability.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a statement is required",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()

	project := r.URL.Query().Get("project")
	target := strings.Join(req.Path, "/")
	opID := s.logs.StartOperation("query", target, project)

	var listing Listing
	var err error
	if statement != "" {
		listing, err = executor.Query(ctx, project, req.Path, statement)
	} else {
		listing, err = builder.Build(ctx, project, req.Path, req.Values)
	}
	if err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		// The statement is deliberately not logged. It is the user's text and
		// may carry a literal they would not choose to keep; the operations
		// ledger records that a query ran and what the backend said about it.
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: target,
			OperationID: opID, Message: "query failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: target,
		OperationID: opID, Message: fmt.Sprintf("query returned %d rows", len(listing.Items)),
	})
	if listing.Items == nil {
		listing.Items = []Resource{}
	}
	if listing.Columns == nil {
		listing.Columns = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"listing": listing, "operation": opID})
}

// SetPlayground configures the local AI playground.
//
// Absent configuration the screen is not offered at all, which is the
// requirement: missing AI must not break the console.
func (s *Server) SetPlayground(p *Playground) {
	if p == nil {
		p = &Playground{}
	}
	s.playground = p
}

// NodeMetrics is one node's measured utilisation.
//
// Capacity comes from the node object and usage from the kubelet's own
// summary, so both halves are the cluster's numbers rather than an estimate
// made here.
type NodeMetrics struct {
	Name string `json:"name"`
	// CPUUsedCores is instantaneous usage, not an average over a window.
	CPUUsedCores     float64 `json:"cpuUsedCores"`
	CPUCapacityCores float64 `json:"cpuCapacityCores"`
	MemoryUsedBytes  int64   `json:"memoryUsedBytes"`
	MemoryTotalBytes int64   `json:"memoryTotalBytes"`
	Pods             int     `json:"pods"`
	Ready            bool    `json:"ready"`
	// CPUCoreNanoSeconds is the kubelet's cumulative counter. A rate computed
	// from two of these is an average over the interval between them, which
	// is a different and more honest number than the instantaneous reading
	// above — and the only one a chart should draw.
	CPUCoreNanoSeconds uint64 `json:"cpuCoreNanoSeconds,omitempty"`
	// At is the kubelet's own timestamp for this reading, not the host's wall
	// clock when it was decoded. A rate divided by the wrong interval is
	// wrong by however long the read took.
	At string `json:"at,omitempty"`
	// NetworkRxBytes and NetworkTxBytes are node-scoped cumulative counters.
	NetworkRxBytes int64 `json:"networkRxBytes,omitempty"`
	NetworkTxBytes int64 `json:"networkTxBytes,omitempty"`
	// FilesystemUsedBytes and FilesystemCapacityBytes describe the node's own
	// filesystem, not any pod's.
	FilesystemUsedBytes     int64 `json:"filesystemUsedBytes,omitempty"`
	FilesystemCapacityBytes int64 `json:"filesystemCapacityBytes,omitempty"`
}

// PodMetrics is one pod's reading, as the kubelet reported it.
//
// Measured on this cluster before being built: all 23 pods on the kind node
// return both cpu and memory, with usageCoreNanoSeconds and the kubelet's own
// time, and so does every container. Recorded in docs/console-verification.md.
type PodMetrics struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// CPUUsedCores is the kubelet's instantaneous usage.
	CPUUsedCores       float64 `json:"cpuUsedCores"`
	CPUCoreNanoSeconds uint64  `json:"cpuCoreNanoSeconds,omitempty"`
	// MemoryWorkingSetBytes is the working set, which is what the kubelet
	// itself uses for eviction decisions.
	MemoryWorkingSetBytes int64  `json:"memoryWorkingSetBytes"`
	At                    string `json:"at,omitempty"`
}

// Key identifies a pod across reads.
func (p PodMetrics) Key() string { return p.Namespace + "/" + p.Name }

// Metrics is the cluster's measured state.
type Metrics struct {
	Nodes []NodeMetrics `json:"nodes"`
	// Pods are the per-pod readings the same kubelet call already returned
	// and which were being counted and thrown away.
	Pods []PodMetrics `json:"pods,omitempty"`
	// Collected is when these numbers were read, so a stalled panel is
	// visible as stale rather than as current.
	Collected string `json:"collected"`
	// Unavailable explains why there are no numbers, which is different from
	// a cluster that is idle.
	Unavailable string `json:"unavailable,omitempty"`
}

// MetricsSource reads cluster utilisation.
type MetricsSource func(ctx context.Context) Metrics

// SetMetrics installs the source for the dashboard's utilisation panel.
func (s *Server) SetMetrics(src MetricsSource) { s.metrics = src }

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, Metrics{
			Unavailable: "cluster metrics are not configured for this instance",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	writeJSON(w, http.StatusOK, s.metrics(ctx))
}

// handleDetail lists what is inside one resource.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("service")
	p, ok := s.providers[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no such service %q", id),
		})
		return
	}
	driller, ok := p.(Driller)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": fmt.Sprintf("%s rows cannot be opened", p.Title()),
		})
		return
	}
	// Repeated rather than slash-joined, so a segment containing a slash — an
	// object key, which routinely does — survives the round trip without a
	// second escaping convention on top of the URL's own.
	var path []string
	for _, segment := range r.URL.Query()["name"] {
		if segment = strings.TrimSpace(segment); segment != "" {
			path = append(path, segment)
		}
	}
	if len(path) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()

	detail, err := driller.Detail(ctx, r.URL.Query().Get("project"), path)
	if err != nil {
		// The provider's own message reaches the screen: which query failed
		// is the useful part, and a generic error would hide it.
		writeJSON(w, http.StatusOK, Detail{Unavailable: userMessage(err)})
		return
	}
	// The page's own actions. Attached here rather than left to each
	// provider's Detail so that the list the page draws and the list the
	// action route checks against are the same call, and a provider cannot
	// offer one it will then refuse.
	if actor, ok := p.(PathActor); ok && detail.Actions == nil {
		detail.Actions = actor.DetailActions(ctx, r.URL.Query().Get("project"), path)
	}
	// Whether this resource has a value to show is the provider's answer, and
	// the same call the reveal route makes — so the button and the route agree
	// by construction rather than by two lists being kept in step.
	if revealer, ok := p.(Revealer); ok && detail.Reveal == "" && revealer.CanReveal(path) {
		detail.Reveal = "Show value"
	}
	// The form for this resource, from the same call the query route validates
	// against — so a control the page draws is one the route will accept.
	if b, ok := p.(Builder); ok && detail.Query == nil {
		if label, fields := b.QueryForm(path); len(fields) > 0 {
			detail.Query = &QuerySpec{Label: label, Fields: fields}
		}
	}
	// Collections are arrays rather than null, so a client that iterates
	// before checking does not fall over on top of the failure it was about
	// to report.
	if detail.Sections == nil {
		detail.Sections = []Section{}
	}
	for i := range detail.Sections {
		if detail.Sections[i].Listing.Items == nil {
			detail.Sections[i].Listing.Items = []Resource{}
		}
		if detail.Sections[i].Listing.Columns == nil {
			detail.Sections[i].Listing.Columns = []string{}
		}
	}
	writeJSON(w, http.StatusOK, detail)
}
