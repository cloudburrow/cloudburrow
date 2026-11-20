package main

// Cloud Run functions console parity (#1853). Google's function page has
// the tabs Metrics, Details, Source, Variables, Trigger(s), Permissions,
// Logs and Testing, and Edit, Copy function and Delete beside its title
// (https://docs.cloud.google.com/functions/1stgendocs/deploy,
// https://docs.cloud.google.com/run/docs/deploy-functions). Each tab here
// reads the function through the official client of the API that serves
// its generation (v2 for 2nd gen, v1 for 1st gen):
//
//   - Source: the files of the archive GenerateDownloadUrl returns, and
//     Edit source, Google's inline editor ending in Save and redeploy: the
//     files are zipped, uploaded to a GenerateUploadUrl URL and sent with
//     UpdateFunction (build_config.source, runtime and entry point).
//   - Triggers: the HTTP URL or the function's own event trigger (Edit
//     trigger, UpdateFunction's event_trigger), and the Eventarc triggers
//     whose destination is the function or its Cloud Run service, with Add
//     trigger and Delete trigger through the official Eventarc client.
//   - Testing: Google's Test the function panel, a JSON body and the curl
//     (2nd gen) or gcloud functions call (1st gen) command; Test the
//     function POSTs the body to the function's URL, as the curl does, or
//     calls the v1 API's CallFunction for 1st gen.
//   - Copy function: the function's archive and settings, created again
//     under a new name.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionsv1 "cloud.google.com/go/functions/apiv1"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	functionsCopyAction          = "copy-function"
	functionsEditSourceAction    = "edit-source"
	functionsEditTriggerAction   = "edit-trigger"
	functionsAddTriggerAction    = "add-trigger"
	functionsDeleteTriggerAction = "delete-trigger"
	functionsDeleteAction        = "delete-function"

	functionTriggerFirestore = "Cloud Firestore"
	functionTriggerAudit     = "Cloud Audit Logs"

	functionFirestorePrefix = "google.cloud.firestore.document.v1."
	functionAuditEvent      = "google.cloud.audit.log.v1.written"
	functionPathPatternOp   = "match-path-pattern"

	// functionSourceLimit bounds the archive the Source tab reads.
	functionSourceLimit = 32 << 20
	// functionTestBody is Google's Testing tab's starting body.
	functionTestBody = `{"name":"Hello World"}`
)

// functionFirestoreEvents are Firestore's document events, as Google's
// Firestore trigger offers them.
var functionFirestoreEvents = []string{"written", "created", "updated", "deleted"}

// functionTriggerFields are the trigger's fields, on Create function and
// Write a function (with HTTP) and on Edit trigger and Add trigger
// (without): each shown only while its trigger type is chosen.
func functionTriggerFields(withHTTP bool, def map[string]string) []console.Field {
	events := make([]string, 0, len(functionStorageEvents))
	for _, e := range functionStorageEvents {
		events = append(events, e.v1)
	}
	types := []string{functionTriggerPubSub, functionTriggerStorage, functionTriggerFirestore, functionTriggerAudit}
	if withHTTP {
		types = append([]string{functionTriggerHTTP}, types...)
	}
	val := func(name, fallback string) string {
		if v := def[name]; v != "" {
			return v
		}
		return fallback
	}
	show := func(kind string) string { return "trigger=" + kind }
	return []console.Field{
		{Name: "trigger", Label: "Trigger type", Type: "select", Options: types, Default: val("trigger", types[0]), Section: "Trigger",
			Help: "HTTP, or an Eventarc trigger: Cloud Pub/Sub, Cloud Storage, Cloud Firestore and Cloud Audit Logs events are delivered locally"},
		{Name: "topic", Label: "Cloud Pub/Sub topic", Type: "text", Section: "Trigger", Default: def["topic"], ShowWhen: show(functionTriggerPubSub),
			Help: "A topic ID in this project, or projects/{project}/topics/{topic}"},
		{Name: "bucket", Label: "Bucket", Type: "text", Section: "Trigger", Default: def["bucket"], ShowWhen: show(functionTriggerStorage),
			Help: "The bucket whose objects' events are delivered"},
		{Name: "storageEvent", Label: "Event type", Type: "select", Options: events, Default: val("storageEvent", events[0]), Section: "Trigger",
			ShowWhen: show(functionTriggerStorage), Help: "finalize is an object created or overwritten"},
		{Name: "firestoreEvent", Label: "Event type", Type: "select", Options: functionFirestoreEvents, Default: val("firestoreEvent", "written"),
			OptionLabels: map[string]string{"written": "google.cloud.firestore.document.v1.written", "created": "google.cloud.firestore.document.v1.created",
				"updated": "google.cloud.firestore.document.v1.updated", "deleted": "google.cloud.firestore.document.v1.deleted"},
			Section: "Trigger", ShowWhen: show(functionTriggerFirestore), Help: "written is any create, update or delete"},
		{Name: "database", Label: "Database", Type: "text", Default: val("database", "(default)"), Section: "Trigger", ShowWhen: show(functionTriggerFirestore),
			Help: "The Firestore database whose documents' changes are delivered"},
		{Name: "document", Label: "Document path", Type: "text", Default: def["document"], Section: "Trigger", ShowWhen: show(functionTriggerFirestore),
			Help: "Optional. A document, or a path pattern such as users/{userId}: matched as Eventarc's match-path-pattern"},
		{Name: "serviceName", Label: "Service name", Type: "text", Default: def["serviceName"], Section: "Trigger", ShowWhen: show(functionTriggerAudit),
			Help: "The service whose Admin Activity audit log entries are delivered, such as secretmanager.googleapis.com"},
		{Name: "methodName", Label: "Method name", Type: "text", Default: def["methodName"], Section: "Trigger", ShowWhen: show(functionTriggerAudit),
			Help: "Such as google.cloud.secretmanager.v1.SecretManagerService.CreateSecret"},
		{Name: "resourceName", Label: "Resource name", Type: "text", Default: def["resourceName"], Section: "Trigger", ShowWhen: show(functionTriggerAudit),
			Help: "Optional. The resource, or a path pattern such as projects/p/secrets/*"},
	}
}

// withTriggerFields replaces Create function's trigger fields with
// functionTriggerFields.
func withTriggerFields(fields []console.Field) []console.Field {
	out := make([]console.Field, 0, len(fields)+6)
	added := false
	for _, f := range fields {
		switch f.Name {
		case "trigger", "topic", "bucket", "storageEvent":
			if !added {
				out = append(out, functionTriggerFields(true, nil)...)
				added = true
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// pathFilter is a filter on attribute, a path pattern when value holds one.
func pathFilter(attribute, value string) *functionspb.EventFilter {
	f := &functionspb.EventFilter{Attribute: attribute, Value: value}
	if strings.ContainsAny(value, "*{") {
		f.Operator = functionPathPatternOp
	}
	return f
}

// functionTriggerValues are the trigger fields' values for a function's event
// trigger, to prefill Edit trigger.
func functionTriggerValues(et *functionspb.EventTrigger) map[string]string {
	v := map[string]string{}
	filter := func(attr string) string {
		for _, f := range et.GetEventFilters() {
			if f.GetAttribute() == attr {
				return f.GetValue()
			}
		}
		return ""
	}
	typ := et.GetEventType()
	switch {
	case typ == functionPubSubEvent:
		v["trigger"], v["topic"] = functionTriggerPubSub, et.GetPubsubTopic()
	case strings.HasPrefix(typ, "google.cloud.storage.object.v1."):
		v["trigger"], v["bucket"] = functionTriggerStorage, filter("bucket")
		for _, e := range functionStorageEvents {
			if e.v2 == typ {
				v["storageEvent"] = e.v1
			}
		}
	case strings.HasPrefix(typ, functionFirestorePrefix):
		v["trigger"], v["firestoreEvent"] = functionTriggerFirestore, strings.TrimPrefix(typ, functionFirestorePrefix)
		v["database"], v["document"] = filter("database"), filter("document")
	case typ == functionAuditEvent:
		v["trigger"] = functionTriggerAudit
		v["serviceName"], v["methodName"], v["resourceName"] = filter("serviceName"), filter("methodName"), filter("resourceName")
	}
	return v
}

// functionMemoryLabel is a memory size as Google's list shows it: 256 MiB.
func functionMemoryLabel(m string) string {
	m = strings.TrimSuffix(strings.TrimSuffix(m, "i"), "B")
	if n, ok := strings.CutSuffix(m, "G"); ok {
		return n + " GiB"
	}
	if n, ok := strings.CutSuffix(m, "M"); ok {
		return n + " MiB"
	}
	if m == "" {
		return ""
	}
	return m + " MiB"
}

// functionAuthentication is the list's Authentication: Allow public access
// when allUsers may invoke the function, through its Cloud Run service's
// policy for 2nd gen (roles/run.invoker) and the v1 API's for 1st gen
// (roles/cloudfunctions.invoker).
func (p functionsProvider) functionAuthentication(ctx context.Context, fn *functionspb.Function) string {
	var pol *iampb.Policy
	role := runInvokerRole
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		role = "roles/cloudfunctions.invoker"
		_ = p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			var err error
			pol, err = c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: fn.GetName()})
			return err
		})
	} else if svc := fn.GetServiceConfig().GetService(); svc != "" {
		opts, err := p.clientOptions()
		if err != nil {
			return ""
		}
		c, err := runclient.NewServicesClient(ctx, opts...)
		if err != nil {
			return ""
		}
		defer func() { _ = c.Close() }()
		pol, _ = c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svc})
	}
	if pol == nil {
		return ""
	}
	for _, b := range pol.GetBindings() {
		if b.GetRole() == role && b.GetCondition() == nil && slices.Contains(b.GetMembers(), "allUsers") {
			return runAllowPublic
		}
	}
	return runRequireAuth
}

// functionArchive reads the .zip GenerateDownloadUrl links to.
func (p functionsProvider) functionArchive(ctx context.Context, fn *functionspb.Function) ([]byte, error) {
	l, err := p.downloadSource(ctx, fn)
	if err != nil {
		return nil, err
	}
	link := l.Items[0].Link
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: time.Minute}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the source: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, functionSourceLimit+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read the source: %s %s", resp.Status, strings.TrimSpace(string(data[:min(len(data), 512)])))
	}
	if len(data) > functionSourceLimit {
		return nil, fmt.Errorf("the source is larger than %d MB", functionSourceLimit>>20)
	}
	return data, nil
}

// archiveFiles are a .zip's text files by name; a binary or directory
// entry is left out and counted.
func archiveFiles(data []byte) (map[string]string, int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("the source is not a .zip: %w", err)
	}
	files, skipped := map[string]string{}, 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, functionSourceLimit))
		rc.Close()
		if err != nil {
			return nil, 0, err
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			skipped++
			continue
		}
		files[f.Name] = string(b)
	}
	return files, skipped, nil
}

// sourceSection is the Source tab: each file of the function's archive,
// and Edit source.
func (p functionsProvider) sourceSection(ctx context.Context, fn *functionspb.Function, runtime, entryPoint string) console.Section {
	sec := console.Section{ID: "source", Label: "Source", Note: "Runtime: " + runtime + ". Function entry point: " + entryPoint + "."}
	data, err := p.functionArchive(ctx, fn)
	if err != nil {
		sec.Kind, sec.Unavailable = console.KindText, "cannot read the source: "+err.Error()
		return sec
	}
	files, skipped, err := archiveFiles(data)
	if err != nil {
		sec.Kind, sec.Unavailable = console.KindText, err.Error()
		return sec
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		sec.Tabs = append(sec.Tabs, console.Section{ID: "file-" + strconv.Itoa(i), Label: n, Kind: console.KindText, Text: files[n]})
	}
	if len(names) == 0 {
		sec.Kind, sec.Text = console.KindText, "The source holds no text files."
	}
	if skipped > 0 {
		sec.Note += fmt.Sprintf(" %d binary file(s) are not shown, and Save and redeploy keeps only the files shown.", skipped)
	}
	if a, ok := editSourceAction(fn, runtime, entryPoint, files); ok {
		sec.Actions = []console.Action{a}
	}
	sec.Actions = append(sec.Actions, sourceTabActions()...)
	return sec
}

// editSourceAction is Edit source, Google's inline editor over the
// function's files, ending in Save and redeploy.
func editSourceAction(fn *functionspb.Function, runtime, entryPoint string, files map[string]string) (console.Action, bool) {
	if fn.GetState() == functionspb.Function_DEPLOYING {
		return console.Action{}, false
	}
	def, _ := json.Marshal(files)
	return console.Action{ID: functionsEditSourceAction, Label: "Edit source", Submit: "Save and redeploy", Fields: []console.Field{
		{Name: "runtime", Label: "Runtime", Type: "text", Required: true, Default: runtime,
			Help: "One of the runtimes ListRuntimes names, such as nodejs22, python312 or go123"},
		{Name: "entryPoint", Label: "Function entry point", Type: "text", Required: true, Default: entryPoint},
		{Name: runSourceFilesField, Label: "Files", Type: "sourcefiles", Required: true, Default: string(def),
			Help: "Add file, rename or delete files and edit them; Save and redeploy uploads them and rebuilds the function"},
	}}, true
}

// uploadArchive uploads a .zip to a new GenerateUploadUrl URL of the
// function's generation in parent, and returns the URL and, for 2nd gen,
// the storage source.
func (p functionsProvider) uploadArchive(ctx context.Context, gen1 bool, parent string, data []byte) (string, *functionspb.StorageSource, error) {
	if gen1 {
		var u string
		err := p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
			if err != nil {
				return err
			}
			u = up.GetUploadUrl()
			return putFunctionSource(ctx, u, data)
		})
		return u, nil, err
	}
	var u string
	var ss *functionspb.StorageSource
	err := p.withClient(ctx, func(c *functions.FunctionClient) error {
		up, err := c.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
		if err != nil {
			return err
		}
		u, ss = up.GetUploadUrl(), up.GetStorageSource()
		return putFunctionSource(ctx, u, data)
	})
	return u, ss, err
}

func functionParent(name string) string {
	return name[:strings.Index(name, "/functions/")]
}

// saveAndRedeploy is Edit source's submit: the files zipped, uploaded and
// sent with UpdateFunction. The function redeploys; its page shows it.
func (p functionsProvider) saveAndRedeploy(ctx context.Context, fn *functionspb.Function, v map[string]string) error {
	data, err := zipSourceFiles(v[runSourceFilesField])
	if err != nil {
		return err
	}
	runtime, entry := strings.TrimSpace(v["runtime"]), strings.TrimSpace(v["entryPoint"])
	if runtime == "" || entry == "" {
		return errors.New("the runtime and the function entry point are required")
	}
	gen1 := fn.GetEnvironment() == functionspb.Environment_GEN_1
	u, ss, err := p.uploadArchive(ctx, gen1, functionParent(fn.GetName()), data)
	if err != nil {
		return err
	}
	if gen1 {
		return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			_, err := c.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: fn.GetName(), Runtime: runtime, EntryPoint: entry,
				SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: u}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"sourceUploadUrl", "runtime", "entryPoint"}}})
			return err
		})
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		_, err := c.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{Function: &functionspb.Function{Name: fn.GetName(),
			BuildConfig: &functionspb.BuildConfig{Runtime: runtime, EntryPoint: entry,
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"build_config.source", "build_config.runtime", "build_config.entry_point"}}})
		return err
	})
}

// eventarcClient is the official Eventarc client on the Run port.
func (p functionsProvider) eventarcClient(ctx context.Context) (*eventarc.Client, error) {
	opts, err := p.clientOptions()
	if err != nil {
		return nil, err
	}
	c, err := eventarc.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to Eventarc: %w", err)
	}
	return c, nil
}

// eventarcTriggers are the Eventarc triggers in the function's region
// whose destination is the function or its Cloud Run service.
func (p functionsProvider) eventarcTriggers(ctx context.Context, fn *functionspb.Function) ([]*eventarcpb.Trigger, error) {
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	svc := lastSegment(fn.GetServiceConfig().GetService())
	var out []*eventarcpb.Trigger
	it := c.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: functionParent(fn.GetName())})
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		d := t.GetDestination()
		if d.GetCloudFunction() == fn.GetName() || (svc != "" && d.GetCloudRun().GetService() == svc) {
			out = append(out, t)
		}
	}
}

// eventFilterText is a trigger's filters other than its type.
func eventFilterText(filters []*functionspb.EventFilter) string {
	var out []string
	for _, f := range filters {
		s := f.GetAttribute() + ": " + f.GetValue()
		if f.GetOperator() != "" {
			s += " (" + f.GetOperator() + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// triggersSection is the Triggers tab.
func (p functionsProvider) triggersSection(ctx context.Context, fn *functionspb.Function, url string, et *functionspb.EventTrigger) console.Section {
	sec := console.Section{ID: "triggers", Label: "Triggers", Listing: console.Listing{
		Columns:    []string{"Trigger type", "Event type", "Filters", "Region", "Destination", "Transport topic"},
		NameColumn: "Trigger", Noun: "triggers", Items: []console.Resource{},
	}}
	region := strings.Split(fn.GetName(), "/")[3]
	if url != "" || et == nil {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: functionTriggerHTTP, Link: url, Fields: map[string]string{
			"Trigger type": functionTriggerHTTP, "Event type": "-", "Filters": "URL: " + orDash(url), "Region": region, "Destination": "the function"}})
	}
	if et != nil && et.GetEventType() != "" {
		name := orDash(lastSegment(et.GetTrigger()))
		filters := eventFilterText(et.GetEventFilters())
		if t := et.GetPubsubTopic(); t != "" {
			filters = strings.TrimPrefix(filters+", topic: "+t, ", ")
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: name, Fields: map[string]string{
			"Trigger type": "Function event trigger", "Event type": et.GetEventType(), "Filters": filters,
			"Region": orDash(et.GetTriggerRegion()), "Destination": "the function"}})
	}
	gen2 := fn.GetEnvironment() != functionspb.Environment_GEN_1
	if gen2 {
		ts, err := p.eventarcTriggers(ctx, fn)
		if err != nil {
			sec.Note = "Eventarc triggers cannot be read: " + err.Error()
		}
		for _, t := range ts {
			typ, filters := "", []*functionspb.EventFilter{}
			for _, f := range t.GetEventFilters() {
				if f.GetAttribute() == "type" {
					typ = f.GetValue()
				} else {
					filters = append(filters, &functionspb.EventFilter{Attribute: f.GetAttribute(), Value: f.GetValue(), Operator: f.GetOperator()})
				}
			}
			dest := "the function"
			if r := t.GetDestination().GetCloudRun(); r != nil {
				dest = "Cloud Run service " + r.GetService() + r.GetPath()
			}
			sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(t.GetName()), Fields: map[string]string{
				"Trigger type": "Eventarc", "Event type": typ, "Filters": eventFilterText(filters),
				"Region": strings.Split(t.GetName(), "/")[3], "Destination": dest,
				"Transport topic": lastSegment(t.GetTransport().GetPubsub().GetTopic())}})
		}
		sec.Actions = p.triggerActions(fn)
		if sec.Note == "" {
			sec.Note = "Each event reaches the function as a CloudEvent (binary content mode) over a Pub/Sub push subscription Eventarc makes. " +
				"Cloud Pub/Sub, Cloud Storage, Cloud Firestore and Cloud Audit Logs (Admin Activity) events are delivered locally."
		}
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// triggerActions are a 2nd-gen function's Edit trigger (its own event
// trigger), Add trigger and Delete trigger (Eventarc triggers to it).
func (p functionsProvider) triggerActions(fn *functionspb.Function) []console.Action {
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		return nil
	}
	var acts []console.Action
	if et := fn.GetEventTrigger(); et != nil && fn.GetState() != functionspb.Function_DEPLOYING {
		acts = append(acts, console.Action{ID: functionsEditTriggerAction, Label: "Edit trigger", Submit: "Save",
			Fields: functionTriggerFields(false, functionTriggerValues(et))})
	}
	add := append([]console.Field{{Name: "name", Label: "Trigger name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{0,62}$`,
		Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."}}, functionTriggerFields(false, nil)...)
	for i := range add {
		if add[i].Name == "topic" {
			add[i].Help = "A topic ID in this project, or projects/{project}/topics/{topic}; empty, Eventarc makes one"
		}
	}
	add = append(add, console.Field{Name: "serviceAccount", Label: "Service account", Type: "text",
		Help: "Optional. The account the delivery authenticates as; the default compute account when empty."})
	return append(acts,
		console.Action{ID: functionsAddTriggerAction, Label: "Add trigger", Submit: "Save trigger", Fields: add},
		console.Action{ID: functionsDeleteTriggerAction, Label: "Delete trigger", Destructive: true, Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true, Help: "An Eventarc trigger on this tab."}}})
}

// editTrigger is Edit trigger: UpdateFunction with event_trigger.
func (p functionsProvider) editTrigger(ctx context.Context, fn *functionspb.Function, v map[string]string) error {
	if fn.GetEventTrigger() == nil {
		return errors.New("an HTTP function's trigger cannot be changed to an event trigger")
	}
	project := strings.Split(fn.GetName(), "/")[1]
	t, err := functionTriggerOf(project, v)
	if err != nil {
		return err
	}
	et := t.v2()
	if et == nil {
		return errors.New("an event function's trigger cannot be changed to HTTP")
	}
	et.RetryPolicy = fn.GetEventTrigger().GetRetryPolicy()
	et.ServiceAccountEmail = fn.GetEventTrigger().GetServiceAccountEmail()
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		_, err := c.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{Function: &functionspb.Function{Name: fn.GetName(), EventTrigger: et},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"event_trigger"}}})
		return err
	})
}

// actOnEventarc performs Add trigger and Delete trigger.
func (p functionsProvider) actOnEventarc(ctx context.Context, fn *functionspb.Function, action string, v map[string]string) error {
	if fn.GetEnvironment() == functionspb.Environment_GEN_1 {
		return errors.New("an Eventarc trigger delivers to a 2nd gen function; a 1st gen function has its one trigger")
	}
	id := strings.TrimSpace(v["name"])
	if id == "" {
		return errors.New("a trigger name is required")
	}
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	parent := functionParent(fn.GetName())
	if action == functionsDeleteTriggerAction {
		ts, err := p.eventarcTriggers(ctx, fn)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(ts, func(t *eventarcpb.Trigger) bool { return t.GetName() == parent+"/triggers/"+id }) {
			return fmt.Errorf("%s is not an Eventarc trigger of function %s", id, lastSegment(fn.GetName()))
		}
		op, err := c.DeleteTrigger(ctx, &eventarcpb.DeleteTriggerRequest{Name: parent + "/triggers/" + id})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	var et *functionspb.EventTrigger
	if strings.TrimSpace(v["trigger"]) == functionTriggerPubSub && strings.TrimSpace(v["topic"]) == "" {
		// No topic: Eventarc makes one, as Google's Create a topic does.
		et = &functionspb.EventTrigger{EventType: functionPubSubEvent}
	} else {
		spec, err := functionTriggerOf(strings.Split(fn.GetName(), "/")[1], v)
		if err != nil {
			return err
		}
		et = spec.v2()
	}
	if et == nil {
		return errors.New("an Eventarc trigger takes an event: choose Cloud Pub/Sub, Cloud Storage, Cloud Firestore or Cloud Audit Logs")
	}
	t := &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: et.GetEventType()}},
		ServiceAccount: strings.TrimSpace(v["serviceAccount"]),
		Destination:    &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: fn.GetName()}}}
	for _, f := range et.GetEventFilters() {
		t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: f.GetAttribute(), Value: f.GetValue(), Operator: f.GetOperator()})
	}
	if topic := et.GetPubsubTopic(); topic != "" {
		t.Transport = &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: topic}}}
	}
	op, err := c.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: id, Trigger: t})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// testingSection is Google's Testing tab: the triggering event and the
// command that sends it.
func testingSection(fn *functionspb.Function, url string, gen1 bool, event bool) console.Section {
	sec := console.Section{ID: "testing", Label: "Testing", Kind: console.KindText}
	parts := strings.Split(fn.GetName(), "/")
	switch {
	case gen1:
		sec.Text = "Test command (gcloud):\n\ngcloud functions call " + parts[5] + " \\\n  --region=" + parts[3] + " \\\n  --data '" + functionTestBody + "'"
	default:
		target := url
		if target == "" {
			target = "FUNCTION_URL"
		}
		sec.Text = "Test command (curl):\n\ncurl -m 70 -X POST " + target + " \\\n  -H \"Authorization: bearer $(gcloud auth print-identity-token)\" \\\n" +
			"  -H \"Content-Type: application/json\" \\\n  -d '" + functionTestBody + "'"
		if url == "" {
			sec.Text += "\n\nThe function has no URL yet: it is reached at its URL once it is deployed."
		}
	}
	if event && !gen1 {
		sec.Text += "\n\nAn event function receives CloudEvents: send the event's ce-* headers with its data, as its trigger does."
	}
	sec.Actions = []console.Action{testAction()}
	return sec
}

// testAction is Test the function: the triggering event's JSON body.
func testAction() console.Action {
	return console.Action{ID: functionsTestAction, Label: "Test the function", Submit: "Test the function", Fields: []console.Field{
		{Name: "data", Label: "Triggering event", Type: "textarea", Default: functionTestBody,
			Help: "The request body, sent as application/json: to the function's URL for 2nd gen, as the curl command sends it; through CallFunction for 1st gen, as gcloud functions call sends it"},
	}}
}

// testFunction is Test the function on a 2nd-gen function: a POST of the
// body to its URL, answered with the status and the response.
func testFunction(ctx context.Context, fn *functionspb.Function, data string) (*console.Listing, error) {
	if fn.GetUrl() == "" {
		return nil, errors.New("the function has no URL: it is not deployed")
	}
	if data != "" && !json.Valid([]byte(data)) {
		return nil, errors.New("the triggering event is not valid JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fn.GetUrl(), strings.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call the function: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res := &console.Listing{Columns: []string{"Value"}, NameColumn: "Output", Noun: "outputs", Total: 2, Items: []console.Resource{
		{Name: "Status", Fields: map[string]string{"Value": resp.Status}},
		{Name: "Response", Fields: map[string]string{"Value": string(body)}}}}
	return res, nil
}

// copyAction is Copy function: a new function from this one's source and
// settings, under the name and region given.
func copyAction(id, region string) console.Action {
	return console.Action{ID: functionsCopyAction, Label: "Copy function", Submit: "Create", Fields: []console.Field{
		{Name: "name", Label: "Function name", Type: "text", Required: true, Default: id + "-copy", Pattern: functionNamePattern.String(),
			Help: "Lower case letters, digits and hyphens, starting with a letter, at most 63"},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: region},
	}}
}

// copyFunction creates the copy: the function's archive uploaded again,
// then CreateFunction with its settings, through the API of its
// generation. It is not waited on: the copy's page shows its deploy.
func (p functionsProvider) copyFunction(ctx context.Context, fn *functionspb.Function, v map[string]string) error {
	id, region := strings.TrimSpace(v["name"]), strings.TrimSpace(v["region"])
	if !functionNamePattern.MatchString(id) {
		return fmt.Errorf("function name %q: lower case letters, digits and hyphens, starting with a letter", id)
	}
	if region == "" {
		region = strings.Split(fn.GetName(), "/")[3]
	}
	parent := "projects/" + strings.Split(fn.GetName(), "/")[1] + "/locations/" + region
	data, err := p.functionArchive(ctx, fn)
	if err != nil {
		return err
	}
	gen1 := fn.GetEnvironment() == functionspb.Environment_GEN_1
	u, ss, err := p.uploadArchive(ctx, gen1, parent, data)
	if err != nil {
		return err
	}
	if gen1 {
		return p.withV1Client(ctx, func(c *functionsv1.CloudFunctionsClient) error {
			src, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: fn.GetName()})
			if err != nil {
				return err
			}
			cp := proto.Clone(src).(*fv1.CloudFunction)
			cp.Name, cp.SourceCode = parent+"/functions/"+id, &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: u}
			cp.Status, cp.UpdateTime, cp.VersionId, cp.BuildId, cp.BuildName = fv1.CloudFunctionStatus_CLOUD_FUNCTION_STATUS_UNSPECIFIED, nil, 0, "", ""
			if ht := cp.GetHttpsTrigger(); ht != nil {
				cp.Trigger = &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{SecurityLevel: ht.GetSecurityLevel()}}
			}
			_, err = c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: cp})
			return err
		})
	}
	sc := proto.Clone(fn.GetServiceConfig()).(*functionspb.ServiceConfig)
	if sc != nil {
		sc.Service, sc.Uri, sc.Revision = "", "", ""
	}
	bc := proto.Clone(fn.GetBuildConfig()).(*functionspb.BuildConfig)
	bc.Build, bc.Source = "", &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: ss}}
	bc.SourceProvenance, bc.DockerRegistry = nil, functionspb.BuildConfig_DOCKER_REGISTRY_UNSPECIFIED
	var et *functionspb.EventTrigger
	if fn.GetEventTrigger() != nil {
		et = proto.Clone(fn.GetEventTrigger()).(*functionspb.EventTrigger)
		et.Trigger, et.TriggerRegion = "", ""
	}
	return p.withClient(ctx, func(c *functions.FunctionClient) error {
		_, err := c.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
			Description: fn.GetDescription(), Labels: fn.GetLabels(), KmsKeyName: fn.GetKmsKeyName(),
			BuildConfig: bc, ServiceConfig: sc, EventTrigger: et}})
		return err
	})
}

// variablesSection is the Variables tab: the runtime and build
// environment variables and the secrets.
func variablesSection(runtimeEnv, buildEnv map[string]string, secrets console.Listing) console.Section {
	env := func(id, label string, m map[string]string) console.Section {
		l := console.Listing{Columns: []string{"Value"}, NameColumn: "Name", Noun: "variables", Items: []console.Resource{}}
		for k, v := range m {
			l.Items = append(l.Items, console.Resource{Name: k, Fields: map[string]string{"Value": v}})
		}
		sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
		l.Total = len(l.Items)
		return console.Section{ID: id, Label: label, Listing: l}
	}
	return console.Section{ID: "variables", Label: "Variables", Tabs: []console.Section{
		env("runtime", "Runtime environment variables", runtimeEnv),
		env("build", "Build environment variables", buildEnv),
		{ID: "secrets", Label: "Secrets", Listing: secrets},
	}}
}

// functionTabs are Google's function page tabs: Metrics, Details, Source,
// Variables, Triggers, Permissions, Logs, Testing.
func (p functionsProvider) functionTabs(ctx context.Context, fn *functionspb.Function, summary []console.Property,
	runtime, entry, url string, runtimeEnv, buildEnv map[string]string, et *functionspb.EventTrigger) []console.Section {
	gen1 := fn.GetEnvironment() == functionspb.Environment_GEN_1
	return []console.Section{
		runNotLocal("metrics", "Metrics", "A function's metrics (invocations, execution times, memory) are Cloud Monitoring's "+
			"cloudfunctions.googleapis.com and run.googleapis.com metrics, which this instance's Cloud Monitoring does not produce."),
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "General information", Properties: summary}}},
		p.sourceSection(ctx, fn, runtime, entry),
		variablesSection(runtimeEnv, buildEnv, functionSecretsListing(fn.GetServiceConfig())),
		p.triggersSection(ctx, fn, url, et),
		{ID: "permissions", Kind: console.KindPermissions},
		{ID: "logs", Label: "Logs", Kind: console.KindLogs},
		testingSection(fn, url, gen1, et != nil && et.GetEventType() != ""),
	}
}

// v1EventTrigger is a 1st-gen function's trigger in v2 terms, for its
// Triggers tab: the store's event trigger, with the v1 resource.
func v1EventTrigger(fn *fv1.CloudFunction) *functionspb.EventTrigger {
	et := fn.GetEventTrigger()
	if et == nil {
		return nil
	}
	return &functionspb.EventTrigger{EventType: et.GetEventType(), EventFilters: []*functionspb.EventFilter{{Attribute: "resource", Value: et.GetResource()}}}
}
