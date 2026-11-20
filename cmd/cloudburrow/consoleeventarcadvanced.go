package main

// Eventarc Advanced's console pages (#1947), beside the Triggers page in
// Eventarc's menu, as Google's console has them: Buses
// (/eventarc/message-buses), Pipelines, Enrollments, Google API sources
// and Channels. Each lists the project's resources in every location,
// creates one with Google's fields, shows what its Get reads, and deletes
// it, through the official Eventarc client on this instance's Eventarc;
// Buses and Enrollments are edited in place. Events flow for real on this
// instance: a message published to a bus reaches each enrollment whose
// CEL match accepts it, and the enrollment's pipeline transforms it and
// delivers it to its destination.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// eaAdvNote is what every Eventarc Advanced page says is served.
const eaAdvNote = "Eventarc Advanced runs on this instance: a message published to a bus (the Publishing API, or a Google API " +
	"source's audit-log events) reaches each enrollment whose CEL match accepts it, and the enrollment's pipeline decodes it " +
	"in its input format, runs its transformation, encodes it in the output format and delivers it, with its retry policy, to " +
	"an HTTP endpoint (a Cloud Run service of this instance among them), a Pub/Sub topic or another bus. Workflows " +
	"destinations, OAuth-token authentication and avro_message publishing are not served."

// eaIDPattern is the ID Google documents for a bus, pipeline, enrollment
// and Google API source.
const eaIDPattern = `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`

// eaSeverities are LoggingConfig's log severities, as Google's Log
// severity select names them.
var eaSeverities = func() []string {
	out := []string{}
	for i := int32(1); i <= int32(eventarcpb.LoggingConfig_EMERGENCY); i++ {
		out = append(out, eventarcpb.LoggingConfig_LogSeverity_name[i])
	}
	return out
}()

const eaTimeFormat = "Jan 2, 2006, 3:04:05 PM UTC"

// eaAdvBase is what every Eventarc Advanced page shares: the Run port's
// Eventarc and the instance's location.
type eaAdvBase struct{ run runProvider }

func (b eaAdvBase) project(project string) string { return b.run.projectOr(project) }

func (b eaAdvBase) client(ctx context.Context) (*eventarc.Client, error) {
	return b.run.eventarcClient(ctx)
}

func (b eaAdvBase) parent(project string) string {
	return "projects/" + b.project(project) + "/locations/" + b.run.location()
}

func (b eaAdvBase) allLocations(project string) string {
	return "projects/" + b.project(project) + "/locations/-"
}

// region checks the form's region, which must be the instance's location.
func (b eaAdvBase) region(values map[string]string) (string, error) {
	r := strings.TrimSpace(values["region"])
	if r == "" {
		r = b.run.location()
	}
	if r != b.run.location() {
		return "", fmt.Errorf("region %q is not served: this instance's location is %s", r, b.run.location())
	}
	return r, nil
}

func (b eaAdvBase) regionField() console.Field {
	return console.Field{Name: "region", Label: "Region", Type: "select", Options: []string{b.run.location()}, Default: b.run.location(), Section: "Details"}
}

// eaLabelsField is Google's Labels control.
func eaLabelsField(def map[string]string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Section: "Labels", Default: console.FormatMap(def),
		KeyPattern: `^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`, MaxEntries: 64}
}

func eaSeverityField(def eventarcpb.LoggingConfig_LogSeverity) console.Field {
	d := "NONE"
	if def != eventarcpb.LoggingConfig_LOG_SEVERITY_UNSPECIFIED {
		d = def.String()
	}
	return console.Field{Name: "logSeverity", Label: "Log severity", Type: "select", Options: eaSeverities, Default: d, Section: "Logging",
		Help: "The lowest severity of the platform logs written to Cloud Logging; NONE writes none."}
}

// eaFormLogging is the form's logging config.
func eaFormLogging(values map[string]string) (*eventarcpb.LoggingConfig, error) {
	s := strings.TrimSpace(values["logSeverity"])
	if s == "" {
		return nil, nil
	}
	v, ok := eventarcpb.LoggingConfig_LogSeverity_value[s]
	if !ok || v == 0 {
		return nil, fmt.Errorf("log severity %q is not one of %v", s, eaSeverities)
	}
	return &eventarcpb.LoggingConfig{LogSeverity: eventarcpb.LoggingConfig_LogSeverity(v)}, nil
}

func eaSeverityText(c *eventarcpb.LoggingConfig) string {
	if c.GetLogSeverity() == eventarcpb.LoggingConfig_LOG_SEVERITY_UNSPECIFIED {
		return "NONE"
	}
	return c.GetLogSeverity().String()
}

func eaKeyText(k string) string {
	if k == "" {
		return "Google-managed"
	}
	return "Cloud KMS key: " + lastSegment(k)
}

func eaLabelsText(m map[string]string) string {
	out := []string{}
	for k, v := range m {
		out = append(out, k+": "+v)
	}
	sort.Strings(out)
	return orDash(strings.Join(out, ", "))
}

func eaStampText(t interface{ AsTime() time.Time }) string { return t.AsTime().Format(eaTimeFormat) }

// eaFullName is a resource the form names by ID or by its full name.
func eaFullName(project, region, collection, v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "projects/") {
		return v
	}
	return "projects/" + project + "/locations/" + region + "/" + collection + "/" + v
}

// eaListAll reads every page of an iterator.
func eaListAll[T any](next func() (T, error)) ([]T, error) {
	var out []T
	for {
		x, err := next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
}

// eaFind is the resource whose ID the row names.
func eaFind[T interface{ GetName() string }](items []T, id, noun string) (T, error) {
	for _, x := range items {
		if lastSegment(x.GetName()) == id {
			return x, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("%s %q not found", noun, id)
}

func eaProps(heading string, props []console.Property, summary int) console.Detail {
	if summary > len(props) {
		summary = len(props)
	}
	return console.Detail{Summary: props[:summary], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: heading, Properties: props}}}}}
}

// --- Buses.

type eaBusesProvider struct{ eaAdvBase }

func (eaBusesProvider) ID() string    { return "eventarc-buses" }
func (eaBusesProvider) Title() string { return "Buses" }

func (p eaBusesProvider) buses(ctx context.Context, project string) ([]*eventarcpb.MessageBus, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	it := c.ListMessageBuses(ctx, &eventarcpb.ListMessageBusesRequest{Parent: p.allLocations(project)})
	return eaListAll(it.Next)
}

func (p eaBusesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "buses", Note: eaAdvNote, Items: []console.Resource{},
		Columns: []string{"Region", "Log severity", "Encryption", "Created"}}
	bs, err := p.buses(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read the message buses: " + consoleErr(err)
		return l, nil
	}
	for _, b := range bs {
		l.Items = append(l.Items, console.Resource{Name: lastSegment(b.GetName()), Status: "Active", Fields: map[string]string{
			"Region": eaRegion(b.GetName()), "Log severity": eaSeverityText(b.GetLoggingConfig()),
			"Encryption": eaKeyText(b.GetCryptoKeyName()), "Created": stampAge(b.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p eaBusesProvider) busFields(b *eventarcpb.MessageBus) []console.Field {
	return []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: b.GetDisplayName(), Section: "Details"},
		eaSeverityField(b.GetLoggingConfig().GetLogSeverity()),
		{Name: "cryptoKey", Label: "Cloud KMS key", Type: "text", Default: b.GetCryptoKeyName(), Section: "Encryption",
			Pattern: `^(projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+)?$`,
			Help:    "Optional. A customer-managed key, projects/P/locations/L/keyRings/R/cryptoKeys/K; empty for a Google-managed key."},
		eaLabelsField(b.GetLabels()),
	}
}

func (p eaBusesProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Bus name", Type: "text", Required: true, Pattern: eaIDPattern, Section: "Details",
			Help: "Lowercase letters, digits and hyphens, starting with a letter, at most 63."},
		p.regionField(),
	}
	return "Create bus", append(fields, p.busFields(nil)...)
}

func (eaBusesProvider) CreateOnPage() bool   { return true }
func (eaBusesProvider) CreateTitle() string  { return "Create bus" }
func (eaBusesProvider) CreateSubmit() string { return "Create" }

// eaFormBus is the form's bus.
func eaFormBus(values map[string]string) (*eventarcpb.MessageBus, error) {
	b := &eventarcpb.MessageBus{DisplayName: strings.TrimSpace(values["displayName"]), CryptoKeyName: strings.TrimSpace(values["cryptoKey"])}
	var err error
	if b.LoggingConfig, err = eaFormLogging(values); err != nil {
		return nil, err
	}
	if b.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return nil, err
	}
	return b, nil
}

func (p eaBusesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.project(project) == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	if _, err := p.region(values); err != nil {
		return "", err
	}
	b, err := eaFormBus(values)
	if err != nil {
		return "", err
	}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	id := strings.TrimSpace(values["name"])
	op, err := c.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: p.parent(project), MessageBusId: id, MessageBus: b})
	if err != nil {
		return "", err
	}
	_, err = op.Wait(ctx)
	return id, err
}

func (p eaBusesProvider) find(ctx context.Context, project, id string) (*eventarcpb.MessageBus, error) {
	bs, err := p.buses(ctx, project)
	if err != nil {
		return nil, err
	}
	return eaFind(bs, id, "bus")
}

func (p eaBusesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	b, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the bus: " + consoleErr(err)}, nil
	}
	d := eaProps("Bus", []console.Property{
		{Label: "Bus name", Value: lastSegment(b.GetName())},
		{Label: "Resource name", Value: b.GetName()},
		{Label: "Region", Value: eaRegion(b.GetName())},
		{Label: "Display name", Value: orDash(b.GetDisplayName())},
		{Label: "Log severity", Value: eaSeverityText(b.GetLoggingConfig())},
		{Label: "Encryption", Value: eaKeyText(b.GetCryptoKeyName())},
		{Label: "Labels", Value: eaLabelsText(b.GetLabels())},
		{Label: "Created", Value: eaStampText(b.GetCreateTime())},
		{Label: "Updated", Value: eaStampText(b.GetUpdateTime())},
		{Label: "Etag", Value: orDash(b.GetEtag())},
	}, 5)
	// The bus's enrollments, as Google's bus page lists them.
	if c, err := p.client(ctx); err == nil {
		defer func() { _ = c.Close() }()
		ens := console.Listing{NameColumn: "Enrollment", Noun: "enrollments", Columns: []string{"CEL match", "Destination pipeline"}, Items: []console.Resource{}}
		all, err := eaListAll(c.ListEnrollments(ctx, &eventarcpb.ListEnrollmentsRequest{Parent: eaLocation(b.GetName())}).Next)
		if err == nil {
			for _, en := range all {
				if en.GetMessageBus() == b.GetName() {
					ens.Items = append(ens.Items, console.Resource{Name: lastSegment(en.GetName()), Fields: map[string]string{
						"CEL match": en.GetCelMatch(), "Destination pipeline": lastSegment(en.GetDestination())}})
				}
			}
			ens.Total = len(ens.Items)
			d.Sections = append(d.Sections, console.Section{ID: "enrollments", Label: "Enrollments", Listing: ens})
		}
	}
	d.Edit = &console.EditForm{Label: "Edit", Title: "Edit bus", Submit: "Save", Fields: p.busFields(b),
		Note: "The bus's name and region cannot change, as on Google."}
	return d, nil
}

func (p eaBusesProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a bus can be edited")
	}
	cur, err := p.find(ctx, project, path[0])
	if err != nil {
		return err
	}
	b, err := eaFormBus(values)
	if err != nil {
		return err
	}
	b.Name, b.Etag = cur.GetName(), cur.GetEtag()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.UpdateMessageBus(ctx, &eventarcpb.UpdateMessageBusRequest{MessageBus: b,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "logging_config", "crypto_key_name", "labels"}}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (p eaBusesProvider) Delete(ctx context.Context, project, row string) error {
	b, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteMessageBus(ctx, &eventarcpb.DeleteMessageBusRequest{Name: b.GetName(), Etag: b.GetEtag()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// eaLocation is the projects/*/locations/* of a name.
func eaLocation(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

// --- Pipelines.

type eaPipelinesProvider struct{ eaAdvBase }

func (eaPipelinesProvider) ID() string    { return "eventarc-pipelines" }
func (eaPipelinesProvider) Title() string { return "Pipelines" }

// eaPipelineDestinations are Google's Destination select.
var eaPipelineDestinations = []string{"HTTP endpoint", "Pub/Sub topic", "Message bus", "Workflows"}

// eaPayloadFormats are the payload format selects.
var eaPayloadFormats = []string{"None", "JSON", "Avro", "Protobuf"}

func (p eaPipelinesProvider) pipelines(ctx context.Context, project string) ([]*eventarcpb.Pipeline, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return eaListAll(c.ListPipelines(ctx, &eventarcpb.ListPipelinesRequest{Parent: p.allLocations(project)}).Next)
}

// eaPipelineDestination is a destination's kind and target.
func eaPipelineDestination(d *eventarcpb.Pipeline_Destination) (string, string) {
	switch {
	case d.GetHttpEndpoint() != nil:
		return "HTTP endpoint", d.GetHttpEndpoint().GetUri()
	case d.GetTopic() != "":
		return "Pub/Sub topic", d.GetTopic()
	case d.GetMessageBus() != "":
		return "Message bus", d.GetMessageBus()
	case d.GetWorkflow() != "":
		return "Workflows", d.GetWorkflow()
	}
	return "", ""
}

func eaFormatText(f *eventarcpb.Pipeline_MessagePayloadFormat) string {
	switch {
	case f.GetJson() != nil:
		return "JSON"
	case f.GetAvro() != nil:
		return "Avro"
	case f.GetProtobuf() != nil:
		return "Protobuf"
	}
	return "None"
}

func (p eaPipelinesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "pipelines", Note: eaAdvNote, Items: []console.Resource{},
		Columns: []string{"Region", "Destination", "Input format", "Created"}}
	ps, err := p.pipelines(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read the pipelines: " + consoleErr(err)
		return l, nil
	}
	for _, pl := range ps {
		dest := "-"
		if ds := pl.GetDestinations(); len(ds) > 0 {
			kind, target := eaPipelineDestination(ds[0])
			dest = kind + ": " + target
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(pl.GetName()), Status: "Active", Fields: map[string]string{
			"Region": eaRegion(pl.GetName()), "Destination": dest, "Input format": eaFormatText(pl.GetInputPayloadFormat()),
			"Created": stampAge(pl.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p eaPipelinesProvider) CreateForm() (string, []console.Field) {
	dst, fmtSec, med, retry := "Destination", "Payload format", "Mediation", "Retry policy"
	return "Create pipeline", []console.Field{
		{Name: "name", Label: "Pipeline name", Type: "text", Required: true, Pattern: eaIDPattern, Section: "Details",
			Help: "Lowercase letters, digits and hyphens, starting with a letter, at most 63."},
		p.regionField(),
		{Name: "displayName", Label: "Display name", Type: "text", Section: "Details"},
		{Name: "destination", Label: "Destination", Type: "select", Options: eaPipelineDestinations, Default: eaPipelineDestinations[0], Section: dst,
			Help: "Where the pipeline delivers each message. Workflows is not served on this instance."},
		{Name: "uri", Label: "Destination URI", Type: "text", Section: dst,
			Help: "HTTP endpoint: an https URI, or the URL of a Cloud Run service of this instance, which is reached through the cluster's ingress."},
		{Name: "networkAttachment", Label: "Network attachment", Type: "text", Section: dst,
			Help: "HTTP endpoint: optional. projects/P/regions/R/networkAttachments/NAME; recorded, not used."},
		{Name: "serviceAccount", Label: "Authentication: Google OIDC service account", Type: "text", Section: dst,
			Help: "HTTP endpoint: optional. The account whose OIDC token each request carries (a Cloud Run service needs one)."},
		{Name: "topic", Label: "Pub/Sub topic", Type: "text", Section: dst, Help: "Pub/Sub topic: the topic ID, or projects/P/topics/T."},
		{Name: "bus", Label: "Message bus", Type: "text", Section: dst, Help: "Message bus: the bus ID in the pipeline's region, or its full name."},
		{Name: "workflow", Label: "Workflow", Type: "text", Section: dst, Help: "Workflows: not served on this instance."},
		{Name: "inputFormat", Label: "Input payload format", Type: "select", Options: eaPayloadFormats, Default: "None", Section: fmtSec},
		{Name: "inputSchema", Label: "Input schema definition", Type: "textarea", Section: fmtSec,
			Help: "Avro or Protobuf: the schema the input data is written in."},
		{Name: "outputFormat", Label: "Output payload format", Type: "select", Options: eaPayloadFormats, Default: "None", Section: fmtSec,
			Help: "None delivers the data in the input format; an output format needs an input format."},
		{Name: "outputSchema", Label: "Output schema definition", Type: "textarea", Section: fmtSec},
		{Name: "transformation", Label: "Transformation (CEL)", Type: "textarea", Section: med,
			Help: `Optional. A CEL expression that builds the new message, such as message.setField("data.seen", true).`},
		{Name: "maxAttempts", Label: "Maximum attempts", Type: "text", Default: "5", Pattern: `^([1-9][0-9]?|100)?$`, Section: retry,
			Help: "1 to 100; 5 by default."},
		{Name: "minRetryDelay", Label: "Minimum retry delay (seconds)", Type: "text", Default: "1", Pattern: `^[0-9]*$`, Section: retry,
			Help: "1 to 600; 1 by default."},
		{Name: "maxRetryDelay", Label: "Maximum retry delay (seconds)", Type: "text", Default: "60", Pattern: `^[0-9]*$`, Section: retry,
			Help: "1 to 600; 60 by default."},
		eaSeverityField(0),
		eaLabelsField(nil),
	}
}

func (eaPipelinesProvider) CreateOnPage() bool   { return true }
func (eaPipelinesProvider) CreateTitle() string  { return "Create pipeline" }
func (eaPipelinesProvider) CreateSubmit() string { return "Create" }

func eaFormFormat(kind, schema string) (*eventarcpb.Pipeline_MessagePayloadFormat, error) {
	schema = strings.TrimSpace(schema)
	switch kind {
	case "", "None":
		return nil, nil
	case "JSON":
		return &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Json{Json: &eventarcpb.Pipeline_MessagePayloadFormat_JsonFormat{}}}, nil
	case "Avro":
		if schema == "" {
			return nil, errors.New("an Avro payload format needs its schema definition")
		}
		return &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Avro{Avro: &eventarcpb.Pipeline_MessagePayloadFormat_AvroFormat{SchemaDefinition: schema}}}, nil
	case "Protobuf":
		if schema == "" {
			return nil, errors.New("a Protobuf payload format needs its schema definition")
		}
		return &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Protobuf{Protobuf: &eventarcpb.Pipeline_MessagePayloadFormat_ProtobufFormat{SchemaDefinition: schema}}}, nil
	}
	return nil, fmt.Errorf("payload format %q is not one of %v", kind, eaPayloadFormats)
}

func eaSeconds(v, name string) (*durationpb.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 600 {
		return nil, fmt.Errorf("%s %q must be 1 to 600 seconds", name, v)
	}
	return durationpb.New(time.Duration(n) * time.Second), nil
}

// eaFormPipeline is Create pipeline's pipeline.
func eaFormPipeline(project, region string, values map[string]string) (*eventarcpb.Pipeline, error) {
	d := &eventarcpb.Pipeline_Destination{}
	switch kind := strings.TrimSpace(values["destination"]); kind {
	case "", "HTTP endpoint":
		uri := strings.TrimSpace(values["uri"])
		if uri == "" {
			return nil, errors.New("an HTTP endpoint destination needs its URI")
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: uri}}
		if na := strings.TrimSpace(values["networkAttachment"]); na != "" {
			d.NetworkConfig = &eventarcpb.Pipeline_Destination_NetworkConfig{NetworkAttachment: na}
		}
		if sa := strings.TrimSpace(values["serviceAccount"]); sa != "" {
			d.AuthenticationConfig = &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_GoogleOidc{
				GoogleOidc: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OidcToken{ServiceAccount: sa}}}
		}
	case "Pub/Sub topic":
		t := strings.TrimSpace(values["topic"])
		if t == "" {
			return nil, errors.New("a Pub/Sub topic destination needs its topic")
		}
		if !strings.HasPrefix(t, "projects/") {
			t = "projects/" + project + "/topics/" + t
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_Topic{Topic: t}
	case "Message bus":
		b := eaFullName(project, region, "messageBuses", values["bus"])
		if b == "" {
			return nil, errors.New("a message bus destination needs its bus")
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_MessageBus{MessageBus: b}
	case "Workflows":
		w := eaFullName(project, region, "workflows", values["workflow"])
		if w == "" {
			return nil, errors.New("a Workflows destination needs its workflow")
		}
		d.DestinationDescriptor = &eventarcpb.Pipeline_Destination_Workflow{Workflow: w}
	default:
		return nil, fmt.Errorf("destination %q is not one of %v", kind, eaPipelineDestinations)
	}
	pl := &eventarcpb.Pipeline{DisplayName: strings.TrimSpace(values["displayName"]), Destinations: []*eventarcpb.Pipeline_Destination{d}}
	var err error
	if pl.InputPayloadFormat, err = eaFormFormat(values["inputFormat"], values["inputSchema"]); err != nil {
		return nil, err
	}
	if d.OutputPayloadFormat, err = eaFormFormat(values["outputFormat"], values["outputSchema"]); err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(values["transformation"]); t != "" {
		pl.Mediations = []*eventarcpb.Pipeline_Mediation{{MediationDescriptor: &eventarcpb.Pipeline_Mediation_Transformation_{
			Transformation: &eventarcpb.Pipeline_Mediation_Transformation{TransformationTemplate: t}}}}
	}
	rp := &eventarcpb.Pipeline_RetryPolicy{}
	if v := strings.TrimSpace(values["maxAttempts"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, fmt.Errorf("maximum attempts %q must be 1 to 100", v)
		}
		rp.MaxAttempts = int32(n)
	}
	if rp.MinRetryDelay, err = eaSeconds(values["minRetryDelay"], "the minimum retry delay"); err != nil {
		return nil, err
	}
	if rp.MaxRetryDelay, err = eaSeconds(values["maxRetryDelay"], "the maximum retry delay"); err != nil {
		return nil, err
	}
	if rp.MaxAttempts != 0 || rp.MinRetryDelay != nil || rp.MaxRetryDelay != nil {
		pl.RetryPolicy = rp
	}
	if pl.LoggingConfig, err = eaFormLogging(values); err != nil {
		return nil, err
	}
	if pl.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return nil, err
	}
	return pl, nil
}

func (p eaPipelinesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.project(project) == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	region, err := p.region(values)
	if err != nil {
		return "", err
	}
	pl, err := eaFormPipeline(p.project(project), region, values)
	if err != nil {
		return "", err
	}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	id := strings.TrimSpace(values["name"])
	op, err := c.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: p.parent(project), PipelineId: id, Pipeline: pl})
	if err != nil {
		return "", err
	}
	_, err = op.Wait(ctx)
	return id, err
}

func (p eaPipelinesProvider) find(ctx context.Context, project, id string) (*eventarcpb.Pipeline, error) {
	ps, err := p.pipelines(ctx, project)
	if err != nil {
		return nil, err
	}
	return eaFind(ps, id, "pipeline")
}

func (p eaPipelinesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	pl, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the pipeline: " + consoleErr(err)}, nil
	}
	kind, target, out, auth, na := "-", "-", "None", "-", "-"
	if ds := pl.GetDestinations(); len(ds) > 0 {
		kind, target = eaPipelineDestination(ds[0])
		out = eaFormatText(ds[0].GetOutputPayloadFormat())
		if sa := ds[0].GetAuthenticationConfig().GetGoogleOidc().GetServiceAccount(); sa != "" {
			auth = "Google OIDC: " + sa
		}
		na = orDash(ds[0].GetNetworkConfig().GetNetworkAttachment())
	}
	tmpl := "-"
	if ms := pl.GetMediations(); len(ms) > 0 {
		tmpl = ms[0].GetTransformation().GetTransformationTemplate()
	}
	rp := pl.GetRetryPolicy()
	attempts, lo, hi := "5", "1s", "60s"
	if rp.GetMaxAttempts() > 0 {
		attempts = strconv.Itoa(int(rp.GetMaxAttempts()))
	}
	if rp.GetMinRetryDelay() != nil {
		lo = rp.GetMinRetryDelay().AsDuration().String()
	}
	if rp.GetMaxRetryDelay() != nil {
		hi = rp.GetMaxRetryDelay().AsDuration().String()
	}
	d := eaProps("Pipeline", []console.Property{
		{Label: "Pipeline name", Value: lastSegment(pl.GetName())},
		{Label: "Resource name", Value: pl.GetName()},
		{Label: "Region", Value: eaRegion(pl.GetName())},
		{Label: "Destination type", Value: kind},
		{Label: "Destination", Value: target},
		{Label: "Network attachment", Value: na},
		{Label: "Authentication", Value: auth},
		{Label: "Input payload format", Value: eaFormatText(pl.GetInputPayloadFormat())},
		{Label: "Output payload format", Value: out},
		{Label: "Transformation", Value: tmpl},
		{Label: "Maximum attempts", Value: attempts},
		{Label: "Minimum retry delay", Value: lo},
		{Label: "Maximum retry delay", Value: hi},
		{Label: "Log severity", Value: eaSeverityText(pl.GetLoggingConfig())},
		{Label: "Display name", Value: orDash(pl.GetDisplayName())},
		{Label: "Labels", Value: eaLabelsText(pl.GetLabels())},
		{Label: "Created", Value: eaStampText(pl.GetCreateTime())},
		{Label: "Updated", Value: eaStampText(pl.GetUpdateTime())},
	}, 5)
	return d, nil
}

func (p eaPipelinesProvider) Delete(ctx context.Context, project, row string) error {
	pl, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeletePipeline(ctx, &eventarcpb.DeletePipelineRequest{Name: pl.GetName(), Etag: pl.GetEtag()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// --- Enrollments.

type eaEnrollmentsProvider struct{ eaAdvBase }

func (eaEnrollmentsProvider) ID() string    { return "eventarc-enrollments" }
func (eaEnrollmentsProvider) Title() string { return "Enrollments" }

func (p eaEnrollmentsProvider) enrollments(ctx context.Context, project string) ([]*eventarcpb.Enrollment, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return eaListAll(c.ListEnrollments(ctx, &eventarcpb.ListEnrollmentsRequest{Parent: p.allLocations(project)}).Next)
}

func (p eaEnrollmentsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "enrollments", Note: eaAdvNote, Items: []console.Resource{},
		Columns: []string{"Region", "Message bus", "CEL match", "Destination pipeline", "Created"}}
	ens, err := p.enrollments(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read the enrollments: " + consoleErr(err)
		return l, nil
	}
	for _, en := range ens {
		l.Items = append(l.Items, console.Resource{Name: lastSegment(en.GetName()), Status: "Active", Fields: map[string]string{
			"Region": eaRegion(en.GetName()), "Message bus": lastSegment(en.GetMessageBus()), "CEL match": en.GetCelMatch(),
			"Destination pipeline": lastSegment(en.GetDestination()), "Created": stampAge(en.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p eaEnrollmentsProvider) fields(buses, pipelines []string, en *eventarcpb.Enrollment, create bool) []console.Field {
	var fields []console.Field
	if create {
		fields = append(fields,
			console.Field{Name: "name", Label: "Enrollment name", Type: "text", Required: true, Pattern: eaIDPattern, Section: "Details",
				Help: "Lowercase letters, digits and hyphens, starting with a letter, at most 63."},
			p.regionField(),
			eaChoiceField("bus", "Message bus", buses, "", "Details", "The bus whose messages the enrollment matches."))
	}
	match := "true"
	if en != nil {
		match = en.GetCelMatch()
	}
	fields = append(fields,
		console.Field{Name: "displayName", Label: "Display name", Type: "text", Default: en.GetDisplayName(), Section: "Details"},
		console.Field{Name: "celMatch", Label: "CEL expression", Type: "textarea", Required: true, Default: match, Section: "Filter",
			Help: `The match on the message, such as message.type == "order.created"; true matches every message.`},
		eaChoiceField("pipeline", "Destination pipeline", pipelines, lastSegment(en.GetDestination()), "Destination",
			"The pipeline the matched messages go to."),
		eaLabelsField(en.GetLabels()))
	return fields
}

// eaChoiceField is a select of the project's resources, or a text field
// when it has none to offer.
func eaChoiceField(name, label string, options []string, def, section, help string) console.Field {
	f := console.Field{Name: name, Label: label, Type: "text", Required: true, Default: def, Section: section, Help: help}
	if len(options) > 0 {
		f.Type, f.Options = "select", options
		if def == "" {
			f.Default = options[0]
		}
	}
	return f
}

func (p eaEnrollmentsProvider) CreateForm() (string, []console.Field) {
	return "Create enrollment", p.fields(nil, nil, nil, true)
}

// CreateFormFor offers the project's buses and pipelines.
func (p eaEnrollmentsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	buses, pipelines := p.choices(ctx, project)
	return p.fields(buses, pipelines, nil, true)
}

func (p eaEnrollmentsProvider) choices(ctx context.Context, project string) (buses, pipelines []string) {
	if bs, err := (eaBusesProvider{p.eaAdvBase}).buses(ctx, project); err == nil {
		for _, b := range bs {
			buses = append(buses, lastSegment(b.GetName()))
		}
	}
	if ps, err := (eaPipelinesProvider{p.eaAdvBase}).pipelines(ctx, project); err == nil {
		for _, pl := range ps {
			pipelines = append(pipelines, lastSegment(pl.GetName()))
		}
	}
	return buses, pipelines
}

func (eaEnrollmentsProvider) CreateOnPage() bool   { return true }
func (eaEnrollmentsProvider) CreateTitle() string  { return "Create enrollment" }
func (eaEnrollmentsProvider) CreateSubmit() string { return "Create" }

// eaFormEnrollment is the form's enrollment.
func eaFormEnrollment(project, region string, values map[string]string) (*eventarcpb.Enrollment, error) {
	en := &eventarcpb.Enrollment{DisplayName: strings.TrimSpace(values["displayName"]), CelMatch: strings.TrimSpace(values["celMatch"]),
		MessageBus: eaFullName(project, region, "messageBuses", values["bus"]), Destination: eaFullName(project, region, "pipelines", values["pipeline"])}
	if en.CelMatch == "" {
		return nil, errors.New("the enrollment needs its CEL expression; true matches every message")
	}
	if en.Destination == "" {
		return nil, errors.New("the enrollment needs its destination pipeline")
	}
	var err error
	if en.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return nil, err
	}
	return en, nil
}

func (p eaEnrollmentsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.project(project) == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	region, err := p.region(values)
	if err != nil {
		return "", err
	}
	en, err := eaFormEnrollment(p.project(project), region, values)
	if err != nil {
		return "", err
	}
	if en.GetMessageBus() == "" {
		return "", errors.New("the enrollment needs its message bus")
	}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	id := strings.TrimSpace(values["name"])
	op, err := c.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: p.parent(project), EnrollmentId: id, Enrollment: en})
	if err != nil {
		return "", err
	}
	_, err = op.Wait(ctx)
	return id, err
}

func (p eaEnrollmentsProvider) find(ctx context.Context, project, id string) (*eventarcpb.Enrollment, error) {
	ens, err := p.enrollments(ctx, project)
	if err != nil {
		return nil, err
	}
	return eaFind(ens, id, "enrollment")
}

func (p eaEnrollmentsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	en, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the enrollment: " + consoleErr(err)}, nil
	}
	d := eaProps("Enrollment", []console.Property{
		{Label: "Enrollment name", Value: lastSegment(en.GetName())},
		{Label: "Resource name", Value: en.GetName()},
		{Label: "Region", Value: eaRegion(en.GetName())},
		{Label: "Message bus", Value: en.GetMessageBus()},
		{Label: "CEL expression", Value: en.GetCelMatch()},
		{Label: "Destination pipeline", Value: en.GetDestination()},
		{Label: "Display name", Value: orDash(en.GetDisplayName())},
		{Label: "Labels", Value: eaLabelsText(en.GetLabels())},
		{Label: "Created", Value: eaStampText(en.GetCreateTime())},
		{Label: "Updated", Value: eaStampText(en.GetUpdateTime())},
		{Label: "Etag", Value: orDash(en.GetEtag())},
	}, 6)
	_, pipelines := p.choices(ctx, project)
	d.Edit = &console.EditForm{Label: "Edit", Title: "Edit enrollment", Submit: "Save", Fields: p.fields(nil, pipelines, en, false),
		Note: "The enrollment's name, region and message bus cannot change, as on Google."}
	return d, nil
}

func (p eaEnrollmentsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only an enrollment can be edited")
	}
	cur, err := p.find(ctx, project, path[0])
	if err != nil {
		return err
	}
	en, err := eaFormEnrollment(p.project(project), eaRegion(cur.GetName()), values)
	if err != nil {
		return err
	}
	en.Name, en.Etag, en.MessageBus = cur.GetName(), cur.GetEtag(), ""
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.UpdateEnrollment(ctx, &eventarcpb.UpdateEnrollmentRequest{Enrollment: en,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "cel_match", "destination", "labels"}}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (p eaEnrollmentsProvider) Delete(ctx context.Context, project, row string) error {
	en, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteEnrollment(ctx, &eventarcpb.DeleteEnrollmentRequest{Name: en.GetName(), Etag: en.GetEtag()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// --- Google API sources.

type eaSourcesProvider struct{ eaAdvBase }

func (eaSourcesProvider) ID() string    { return "eventarc-google-api-sources" }
func (eaSourcesProvider) Title() string { return "Google API sources" }

func (p eaSourcesProvider) sources(ctx context.Context, project string) ([]*eventarcpb.GoogleApiSource, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return eaListAll(c.ListGoogleApiSources(ctx, &eventarcpb.ListGoogleApiSourcesRequest{Parent: p.allLocations(project)}).Next)
}

const eaSourcesNote = "A Google API source puts the Google events of its project (here, the Cloud Audit Logs Admin Activity " +
	"entries of the calls this instance serves, google.cloud.audit.log.v1.written) on its destination bus. " + eaAdvNote

func (p eaSourcesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "Google API sources", Note: eaSourcesNote, Items: []console.Resource{},
		Columns: []string{"Region", "Destination bus", "Log severity", "Created"}}
	ss, err := p.sources(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read the Google API sources: " + consoleErr(err)
		return l, nil
	}
	for _, s := range ss {
		l.Items = append(l.Items, console.Resource{Name: lastSegment(s.GetName()), Status: "Active", Fields: map[string]string{
			"Region": eaRegion(s.GetName()), "Destination bus": lastSegment(s.GetDestination()),
			"Log severity": eaSeverityText(s.GetLoggingConfig()), "Created": stampAge(s.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p eaSourcesProvider) fields(buses []string) []console.Field {
	return []console.Field{
		{Name: "name", Label: "Google API source name", Type: "text", Required: true, Pattern: eaIDPattern, Section: "Details",
			Help: "Lowercase letters, digits and hyphens, starting with a letter, at most 63."},
		p.regionField(),
		{Name: "displayName", Label: "Display name", Type: "text", Section: "Details"},
		eaChoiceField("bus", "Destination bus", buses, "", "Destination", "The bus the project's Google events are published to."),
		eaSeverityField(0),
		eaLabelsField(nil),
	}
}

func (p eaSourcesProvider) CreateForm() (string, []console.Field) {
	return "Create Google API source", p.fields(nil)
}

// CreateFormFor offers the project's buses.
func (p eaSourcesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	var buses []string
	if bs, err := (eaBusesProvider{p.eaAdvBase}).buses(ctx, project); err == nil {
		for _, b := range bs {
			buses = append(buses, lastSegment(b.GetName()))
		}
	}
	return p.fields(buses)
}

func (eaSourcesProvider) CreateOnPage() bool   { return true }
func (eaSourcesProvider) CreateTitle() string  { return "Create Google API source" }
func (eaSourcesProvider) CreateSubmit() string { return "Create" }

// eaFormSource is the form's Google API source.
func eaFormSource(project, region string, values map[string]string) (*eventarcpb.GoogleApiSource, error) {
	s := &eventarcpb.GoogleApiSource{DisplayName: strings.TrimSpace(values["displayName"]),
		Destination: eaFullName(project, region, "messageBuses", values["bus"])}
	if s.Destination == "" {
		return nil, errors.New("the Google API source needs its destination bus")
	}
	var err error
	if s.LoggingConfig, err = eaFormLogging(values); err != nil {
		return nil, err
	}
	if s.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return nil, err
	}
	return s, nil
}

func (p eaSourcesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.project(project) == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	region, err := p.region(values)
	if err != nil {
		return "", err
	}
	s, err := eaFormSource(p.project(project), region, values)
	if err != nil {
		return "", err
	}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	id := strings.TrimSpace(values["name"])
	op, err := c.CreateGoogleApiSource(ctx, &eventarcpb.CreateGoogleApiSourceRequest{Parent: p.parent(project), GoogleApiSourceId: id, GoogleApiSource: s})
	if err != nil {
		return "", err
	}
	_, err = op.Wait(ctx)
	return id, err
}

func (p eaSourcesProvider) find(ctx context.Context, project, id string) (*eventarcpb.GoogleApiSource, error) {
	ss, err := p.sources(ctx, project)
	if err != nil {
		return nil, err
	}
	return eaFind(ss, id, "Google API source")
}

func (p eaSourcesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	s, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the Google API source: " + consoleErr(err)}, nil
	}
	subs := "This project"
	if s.GetOrganizationSubscription().GetEnabled() {
		subs = "The organization's projects"
	} else if l := s.GetProjectSubscriptions().GetList(); len(l) > 0 {
		subs = strings.Join(l, ", ")
	}
	return eaProps("Google API source", []console.Property{
		{Label: "Google API source name", Value: lastSegment(s.GetName())},
		{Label: "Resource name", Value: s.GetName()},
		{Label: "Region", Value: eaRegion(s.GetName())},
		{Label: "Destination bus", Value: s.GetDestination()},
		{Label: "Projects", Value: subs},
		{Label: "Log severity", Value: eaSeverityText(s.GetLoggingConfig())},
		{Label: "Encryption", Value: eaKeyText(s.GetCryptoKeyName())},
		{Label: "Display name", Value: orDash(s.GetDisplayName())},
		{Label: "Labels", Value: eaLabelsText(s.GetLabels())},
		{Label: "Created", Value: eaStampText(s.GetCreateTime())},
		{Label: "Updated", Value: eaStampText(s.GetUpdateTime())},
	}, 5), nil
}

func (p eaSourcesProvider) Delete(ctx context.Context, project, row string) error {
	s, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteGoogleApiSource(ctx, &eventarcpb.DeleteGoogleApiSourceRequest{Name: s.GetName(), Etag: s.GetEtag()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// --- Channels.

type eaChannelsProvider struct{ eaAdvBase }

func (eaChannelsProvider) ID() string    { return "eventarc-channels" }
func (eaChannelsProvider) Title() string { return "Channels" }

const eaChannelsNote = "A channel receives custom events published to it with the Publishing API (PublishEvents), and a " +
	"trigger on it delivers those of its event type. One with no provider is Active at once; one for a provider is Pending, " +
	"with an activation token, until a channel connection made with the token activates it. Every provider is local here: " +
	"whoever holds the token creates the connection on this instance."

func (p eaChannelsProvider) channels(ctx context.Context, project string) ([]*eventarcpb.Channel, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return eaListAll(c.ListChannels(ctx, &eventarcpb.ListChannelsRequest{Parent: p.allLocations(project)}).Next)
}

// eaChannelState is a state as Google's page shows it.
func eaChannelState(s eventarcpb.Channel_State) string {
	switch s {
	case eventarcpb.Channel_ACTIVE:
		return "Active"
	case eventarcpb.Channel_PENDING:
		return "Pending"
	case eventarcpb.Channel_INACTIVE:
		return "Inactive"
	}
	return "-"
}

func (p eaChannelsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "channels", Note: eaChannelsNote, Items: []console.Resource{},
		Columns: []string{"Provider", "State", "Region", "Created"}}
	cs, err := p.channels(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read the channels: " + consoleErr(err)
		return l, nil
	}
	for _, ch := range cs {
		provider := "Custom"
		if ch.GetProvider() != "" {
			provider = lastSegment(ch.GetProvider())
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(ch.GetName()), Status: eaChannelState(ch.GetState()), Fields: map[string]string{
			"Provider": provider, "State": eaChannelState(ch.GetState()), "Region": eaRegion(ch.GetName()), "Created": stampAge(ch.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p eaChannelsProvider) CreateForm() (string, []console.Field) {
	return "Create channel", []console.Field{
		{Name: "name", Label: "Channel name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{0,62}$`, Section: "Details",
			Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."},
		p.regionField(),
		{Name: "provider", Label: "Provider", Type: "text", Section: "Details",
			Help: "Optional. The third-party provider, projects/P/locations/L/providers/ID; empty for your own custom events."},
		{Name: "cryptoKey", Label: "Cloud KMS key", Type: "text", Section: "Encryption",
			Pattern: `^(projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+)?$`, Help: "Optional. A customer-managed key."},
		eaLabelsField(nil),
	}
}

func (eaChannelsProvider) CreateOnPage() bool   { return true }
func (eaChannelsProvider) CreateTitle() string  { return "Create channel" }
func (eaChannelsProvider) CreateSubmit() string { return "Create" }

func (p eaChannelsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.project(project) == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	if _, err := p.region(values); err != nil {
		return "", err
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	ch := &eventarcpb.Channel{Provider: strings.TrimSpace(values["provider"]), CryptoKeyName: strings.TrimSpace(values["cryptoKey"]), Labels: labels}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	id := strings.TrimSpace(values["name"])
	op, err := c.CreateChannel(ctx, &eventarcpb.CreateChannelRequest{Parent: p.parent(project), ChannelId: id, Channel: ch})
	if err != nil {
		return "", err
	}
	_, err = op.Wait(ctx)
	return id, err
}

func (p eaChannelsProvider) find(ctx context.Context, project, id string) (*eventarcpb.Channel, error) {
	cs, err := p.channels(ctx, project)
	if err != nil {
		return nil, err
	}
	return eaFind(cs, id, "channel")
}

func (p eaChannelsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	ch, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the channel: " + consoleErr(err)}, nil
	}
	provider := "Custom"
	if ch.GetProvider() != "" {
		provider = ch.GetProvider()
	}
	return eaProps("Channel", []console.Property{
		{Label: "Channel name", Value: lastSegment(ch.GetName())},
		{Label: "Resource name", Value: ch.GetName()},
		{Label: "Region", Value: eaRegion(ch.GetName())},
		{Label: "Provider", Value: provider},
		{Label: "State", Value: eaChannelState(ch.GetState())},
		{Label: "Activation token", Value: orDash(ch.GetActivationToken())},
		{Label: "Encryption", Value: eaKeyText(ch.GetCryptoKeyName())},
		{Label: "Labels", Value: eaLabelsText(ch.GetLabels())},
		{Label: "Created", Value: eaStampText(ch.GetCreateTime())},
		{Label: "Updated", Value: eaStampText(ch.GetUpdateTime())},
	}, 6), nil
}

func (p eaChannelsProvider) Delete(ctx context.Context, project, row string) error {
	ch, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteChannel(ctx, &eventarcpb.DeleteChannelRequest{Name: ch.GetName()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

var (
	_ console.PageCreator         = eaBusesProvider{}
	_ console.CreateTitler        = eaBusesProvider{}
	_ console.CreateSubmitter     = eaBusesProvider{}
	_ console.Driller             = eaBusesProvider{}
	_ console.Editor              = eaBusesProvider{}
	_ console.Deleter             = eaBusesProvider{}
	_ console.PageCreator         = eaPipelinesProvider{}
	_ console.Driller             = eaPipelinesProvider{}
	_ console.Deleter             = eaPipelinesProvider{}
	_ console.ProjectCreateFormer = eaEnrollmentsProvider{}
	_ console.PageCreator         = eaEnrollmentsProvider{}
	_ console.Driller             = eaEnrollmentsProvider{}
	_ console.Editor              = eaEnrollmentsProvider{}
	_ console.Deleter             = eaEnrollmentsProvider{}
	_ console.ProjectCreateFormer = eaSourcesProvider{}
	_ console.PageCreator         = eaSourcesProvider{}
	_ console.Driller             = eaSourcesProvider{}
	_ console.Deleter             = eaSourcesProvider{}
	_ console.PageCreator         = eaChannelsProvider{}
	_ console.Driller             = eaChannelsProvider{}
	_ console.Deleter             = eaChannelsProvider{}
)
