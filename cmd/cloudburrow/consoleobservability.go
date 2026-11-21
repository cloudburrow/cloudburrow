package main

// The Observability API's settings and buckets (#2117), as two tabs of
// Logging's Settings beside Log Scopes and Trace Scopes, over the
// observability.googleapis.com v1 the Logging port serves, in process:
//
//   - Observability Settings: the project's default storage location (the
//     location of system-created observability buckets, such as _Trace)
//     and its default Cloud KMS key, with the service account the key must
//     be shared with (docs.cloud.google.com/stackdriver/docs/observability/
//     set-defaults-for-observability-buckets, read 2026-11-21);
//   - Observability Buckets: the project's buckets, _Trace among them once
//     Cloud Trace takes spans or it is created, with Create bucket, Edit
//     (display name, description, key, and the dataset's linked BigQuery
//     dataset) and Reprovision; a bucket's page lists its datasets, views
//     and links (docs.cloud.google.com/trace/docs/storage-overview and
//     analytics-query-linked-dataset, read 2026-11-21).
//
// Google's console layout for these is UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/storage-overview):
// the pages describe the API and gcloud, not a console page.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// callObs calls the Observability API in process; out, when not nil, is
// the response (an operation's response for a done operation).
func callObs(a *logging.Admin, method, path string, body any, out proto.Message) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	code, resp := a.CallObservability(method, path, raw)
	if code != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(resp, &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("HTTP %d", code)
	}
	if out == nil {
		return nil
	}
	u := protojson.UnmarshalOptions{DiscardUnknown: true}
	if _, isOp := out.(*longrunningpb.Operation); !isOp {
		var op longrunningpb.Operation
		if u.Unmarshal(resp, &op) == nil && op.GetDone() && op.GetResponse() != nil {
			return op.GetResponse().UnmarshalTo(out)
		}
	}
	return u.Unmarshal(resp, out)
}

// --- settings --------------------------------------------------------------

type observabilitySettingsProvider struct{ loggingAdminScreen }

func (observabilitySettingsProvider) ID() string    { return "observability-settings" }
func (observabilitySettingsProvider) Title() string { return "Observability Settings" }

const observabilitySettingsNote = "The default storage location is where system-created observability buckets, such as Cloud Trace's _Trace, are made. " +
	"A default Cloud KMS key must be in the default storage location, and the service account needs roles/cloudkms.cryptoKeyEncrypterDecrypter on it."

func observabilitySettingsName(project string) string {
	return "projects/" + project + "/locations/global/settings"
}

func (p observabilitySettingsProvider) read(project string) (*obspb.Settings, string, error) {
	a, err := p.admin()
	if err != nil {
		return nil, "", err
	}
	st := &obspb.Settings{}
	if err := callObs(a, http.MethodGet, "/v1/"+url.PathEscape(observabilitySettingsName(project)), nil, st); err != nil {
		return nil, "", err
	}
	key := ""
	if l := st.GetDefaultStorageLocation(); l != "" {
		ks := &obspb.Settings{}
		if err := callObs(a, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/locations/"+url.PathEscape(l)+"/settings", nil, ks); err == nil {
			key = ks.GetKmsKeyName()
		}
	}
	return st, key, nil
}

func (p observabilitySettingsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Default storage location", "Default KMS key", "Service account"}, NameColumn: "Settings",
		Noun: "settings", Note: observabilitySettingsNote}
	if _, err := p.admin(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Observability settings are per project. Choose one in the toolbar."
		return out, nil
	}
	st, key, err := p.read(project)
	if err != nil {
		return console.Listing{}, err
	}
	out.Items = []console.Resource{{Name: st.GetName(), Fields: map[string]string{
		"Default storage location": orDash(st.GetDefaultStorageLocation()), "Default KMS key": orDash(key),
		"Service account": st.GetServiceAccountId()}}}
	out.Total, out.RowsOpenable = 1, true
	return out, nil
}

func (p observabilitySettingsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	if strings.Join(path, "/") != observabilitySettingsName(project) {
		return console.Detail{Unavailable: strings.Join(path, "/") + " is not project " + project + "'s Observability settings"}, nil
	}
	st, key, err := p.read(project)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	return console.Detail{
		Title: "Observability settings",
		Sections: []console.Section{{ID: "settings", Label: "Settings", Kind: console.KindProperties, Note: observabilitySettingsNote,
			Groups: []console.PropertyGroup{{Heading: "Defaults", Properties: []console.Property{
				{Label: "Name", Value: st.GetName()},
				{Label: "Default storage location", Value: orDash(st.GetDefaultStorageLocation())},
				{Label: "Default KMS key", Value: orDash(key)},
				{Label: "Service account", Value: st.GetServiceAccountId()},
			}}}}},
		Edit: &console.EditForm{Label: "Edit settings", Submit: "Save", Fields: []console.Field{
			{Name: "defaultStorageLocation", Label: "Default storage location", Type: "select", Default: st.GetDefaultStorageLocation(),
				Options: append([]string{""}, logging.ObservabilityBucketLocations...),
				Help:    "A supported observability bucket location: us, eu or a region. Empty for none."},
			{Name: "kmsKeyName", Label: "Default KMS key", Type: "text", Default: key,
				Help: "projects/P/locations/L/keyRings/R/cryptoKeys/K, in the default storage location; empty for none."},
		}},
	}, nil
}

// Edit is updateSettings of global's default storage location, then of
// that location's default key.
func (p observabilitySettingsProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if strings.Join(path, "/") != observabilitySettingsName(project) {
		return fmt.Errorf("%s is not project %s's Observability settings", strings.Join(path, "/"), project)
	}
	loc := strings.TrimSpace(values["defaultStorageLocation"])
	if err := callObs(a, http.MethodPatch, "/v1/"+observabilitySettingsName(project)+"?updateMask=defaultStorageLocation",
		map[string]string{"defaultStorageLocation": loc}, nil); err != nil {
		return err
	}
	key := strings.TrimSpace(values["kmsKeyName"])
	keyLoc := loc
	if key != "" {
		if l, ok := cmek.Location(key); ok {
			keyLoc = l
		}
	}
	if keyLoc == "" {
		if key != "" {
			return errors.New("if you set a default Cloud KMS key for a location, then you must also set that location as the default storage location")
		}
		return nil
	}
	return callObs(a, http.MethodPatch, "/v1/projects/"+project+"/locations/"+keyLoc+"/settings?updateMask=kmsKeyName",
		map[string]string{"kmsKeyName": key}, nil)
}

// --- buckets ---------------------------------------------------------------

type observabilityBucketsProvider struct{ loggingAdminScreen }

func (observabilityBucketsProvider) ID() string    { return "observability-buckets" }
func (observabilityBucketsProvider) Title() string { return "Observability Buckets" }

const observabilityBucketsNote = "Observability buckets hold Cloud Trace's spans: _Trace, its Spans dataset and the _AllSpans view. " +
	"The system makes _Trace when trace data first arrives, in the default storage location. A linked BigQuery dataset makes Spans queryable from BigQuery."

const reprovisionAction = "reprovision"

func (p observabilityBucketsProvider) own(project, name string) error {
	if !strings.HasPrefix(name, "projects/"+project+"/locations/") || !strings.Contains(name, "/buckets/") {
		return fmt.Errorf("%s is not an observability bucket of project %s", name, project)
	}
	return nil
}

func (p observabilityBucketsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Location", "Display name", "Description", "KMS key"}, NameColumn: "Bucket", Noun: "observability buckets",
		Note: observabilityBucketsNote}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Observability buckets are kept per project. Choose one in the toolbar."
		return out, nil
	}
	resp := &obspb.ListBucketsResponse{}
	if err := callObs(a, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/locations/-/buckets?pageSize=1000", nil, resp); err != nil {
		return console.Listing{}, err
	}
	for _, b := range resp.GetBuckets() {
		loc := strings.Split(b.GetName(), "/")[3]
		out.Items = append(out.Items, console.Resource{Name: b.GetName(), Fields: map[string]string{"Location": loc,
			"Display name": orDash(b.GetDisplayName()), "Description": orDash(b.GetDescription()), "KMS key": orDash(b.GetCmekSettings().GetKmsKey())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (observabilityBucketsProvider) CreateOnPage() bool   { return true }
func (observabilityBucketsProvider) CreateLayout() string { return console.CreateLayoutPage }
func (observabilityBucketsProvider) CreateSubmit() string { return "Create bucket" }

func (observabilityBucketsProvider) CreateForm() (string, []console.Field) {
	return "Create observability bucket", []console.Field{
		{Name: "id", Label: "Bucket ID", Type: "text", Required: true, Default: logging.TraceBucketID,
			Help: "_Trace holds Cloud Trace's spans; another ID is 1-100 letters, digits, _ and -, not starting with _."},
		{Name: "location", Label: "Location", Type: "select", Required: true, Default: logging.DefaultTraceBucketLocation, Options: logging.ObservabilityBucketLocations},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "kmsKey", Label: "KMS key", Type: "text", Help: "Optional: projects/P/locations/L/keyRings/R/cryptoKeys/K in the bucket's location. Empty takes the location's default key, if any."},
	}
}

func (p observabilityBucketsProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a bucket")
	}
	body := map[string]any{"displayName": values["displayName"], "description": values["description"]}
	if k := strings.TrimSpace(values["kmsKey"]); k != "" {
		body["cmekSettings"] = map[string]string{"kmsKey": k}
	}
	b := &obspb.Bucket{}
	path := "/v1/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(values["location"]) + "/buckets?bucketId=" + url.QueryEscape(strings.TrimSpace(values["id"]))
	if err := callObs(a, http.MethodPost, path, body, b); err != nil {
		return "", err
	}
	return b.GetName(), nil
}

// spansLink is the bucket's Spans dataset and its link, if any.
func (p observabilityBucketsProvider) spansLink(a *logging.Admin, bucket string) (dataset string, link *obspb.Link) {
	ds := &obspb.ListDatasetsResponse{}
	if callObs(a, http.MethodGet, "/v1/"+bucket+"/datasets", nil, ds) != nil || len(ds.GetDatasets()) == 0 {
		return "", nil
	}
	dataset = ds.GetDatasets()[0].GetName()
	ls := &obspb.ListLinksResponse{}
	if callObs(a, http.MethodGet, "/v1/"+dataset+"/links", nil, ls) == nil && len(ls.GetLinks()) > 0 {
		link = ls.GetLinks()[0]
	}
	return dataset, link
}

func (p observabilityBucketsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	name := strings.Join(path, "/")
	if err := p.own(project, name); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	a, err := p.admin()
	if err != nil {
		return console.Detail{}, err
	}
	b := &obspb.Bucket{}
	if err := callObs(a, http.MethodGet, "/v1/"+name, nil, b); err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Name", Value: b.GetName()}, {Label: "Display name", Value: orDash(b.GetDisplayName())},
		{Label: "Description", Value: orDash(b.GetDescription())},
		{Label: "KMS key", Value: orDash(b.GetCmekSettings().GetKmsKey())}, {Label: "KMS key version", Value: orDash(b.GetCmekSettings().GetKmsKeyVersion())},
		{Label: "Created", Value: b.GetCreateTime().AsTime().UTC().Format("2006-01-02 15:04:05 UTC")},
		{Label: "Updated", Value: b.GetUpdateTime().AsTime().UTC().Format("2006-01-02 15:04:05 UTC")},
	}
	var data []console.Property
	dataset, link := p.spansLink(a, name)
	linkID, linkDesc := "", ""
	if dataset != "" {
		data = append(data, console.Property{Label: "Dataset", Value: dataset})
		vs := &obspb.ListViewsResponse{}
		if callObs(a, http.MethodGet, "/v1/"+dataset+"/views", nil, vs) == nil {
			for _, v := range vs.GetViews() {
				data = append(data, console.Property{Label: "View", Value: v.GetName()})
			}
		}
		if link != nil {
			linkID = link.GetName()[strings.LastIndex(link.GetName(), "/")+1:]
			linkDesc = link.GetDescription()
			data = append(data, console.Property{Label: "Linked BigQuery dataset", Value: project + "." + linkID})
		} else {
			data = append(data, console.Property{Label: "Linked BigQuery dataset", Value: "None"})
		}
	} else {
		data = append(data, console.Property{Label: "Datasets", Value: "None"})
	}
	fields := []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: b.GetDisplayName()},
		{Name: "description", Label: "Description", Type: "text", Default: b.GetDescription()},
		{Name: "kmsKey", Label: "KMS key", Type: "text", Default: b.GetCmekSettings().GetKmsKey()},
	}
	if dataset != "" {
		fields = append(fields,
			console.Field{Name: "linkId", Label: "Linked BigQuery dataset", Type: "text", Default: linkID,
				Help: "The linked dataset's name: up to 100 letters, digits and underscores, unique in the project. Empty removes the link and its dataset."},
			console.Field{Name: "linkDescription", Label: "Link description", Type: "text", Default: linkDesc})
	}
	return console.Detail{Title: name, Sections: []console.Section{{ID: "overview", Label: "Details", Note: observabilityBucketsNote, Groups: []console.PropertyGroup{
		{Heading: "Bucket", Properties: props}, {Heading: "Datasets, views and links", Properties: data}}}},
		Edit: &console.EditForm{Label: "Edit bucket", Submit: "Save", Fields: fields}}, nil
}

func (p observabilityBucketsProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	name := strings.Join(path, "/")
	if err := p.own(project, name); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	body := map[string]any{"displayName": values["displayName"], "description": values["description"],
		"cmekSettings": map[string]string{"kmsKey": strings.TrimSpace(values["kmsKey"])}}
	if err := callObs(a, http.MethodPatch, "/v1/"+name+"?updateMask=displayName,description,cmekSettings", body, nil); err != nil {
		return err
	}
	dataset, link := p.spansLink(a, name)
	if dataset == "" {
		return nil
	}
	want := strings.TrimSpace(values["linkId"])
	have := ""
	if link != nil {
		have = link.GetName()[strings.LastIndex(link.GetName(), "/")+1:]
	}
	switch {
	case want == have && link != nil:
		return callObs(a, http.MethodPatch, "/v1/"+link.GetName()+"?updateMask=description", map[string]string{"description": values["linkDescription"]}, nil)
	case want == have:
		return nil
	}
	if link != nil {
		if err := callObs(a, http.MethodDelete, "/v1/"+link.GetName(), nil, nil); err != nil {
			return err
		}
	}
	if want == "" {
		return nil
	}
	return callObs(a, http.MethodPost, "/v1/"+dataset+"/links?linkId="+url.QueryEscape(want), map[string]string{"description": values["linkDescription"]}, nil)
}

func (observabilityBucketsProvider) Actions(console.Resource) []console.Action {
	return []console.Action{{ID: reprovisionAction, Label: "Reprovision", Destructive: true,
		ConfirmTitle: "Reprovision bucket?"}}
}

// Act performs Reprovision, which "is allowed only a single time for a
// bucket and results in data loss".
func (p observabilityBucketsProvider) Act(_ context.Context, project, name, action string) error {
	if action != reprovisionAction {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := p.own(project, name); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	return callObs(a, http.MethodPost, "/v1/"+name+":reprovision", map[string]string{"name": name}, nil)
}

var (
	_ console.Driller         = observabilityBucketsProvider{}
	_ console.Actor           = observabilityBucketsProvider{}
	_ console.CreateSubmitter = observabilityBucketsProvider{}
	_ console.Driller         = observabilitySettingsProvider{}
)
