package pubsubfront

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// IAM enforcement (#1879). With `up --iam-enforcement=on` the front is
// given the host's check address (--iam-check), and each mapped call is
// checked, before it reaches the emulator, by the host's one evaluator
// (internal/iamcheck): the permission Pub/Sub's "Access control with IAM"
// names for the method (https://docs.cloud.google.com/pubsub/docs/access-control,
// read 2026-11-19), on the topic, subscription or snapshot, against that
// resource's own policy (iam.go) and its project's, folders' and
// organization's; a create or a list is checked on the project. A denial is
// PERMISSION_DENIED "User not authorized to perform this action.", Google's
// Pub/Sub text; a bearer token the instance did not mint is UNAUTHENTICATED.
// DetachSubscription is checked on the topic the subscription is attached
// to, and the schema service's methods on the schema or its project, as
// that page names them (#1887); the REST transport's calls are checked as
// their gRPC methods (restAuthorize).

// psCheck is one permission a method needs, on the resource its request
// field names (on that name's project when project is set).
type psCheck struct {
	perm, field string
	project     bool
}

// onTopic are the methods whose subscription field is checked on the
// topic the subscription is attached to.
var onTopic = map[string]bool{publisher + "DetachSubscription": true}

var psChecks = map[string][]psCheck{
	publisher + "CreateTopic":              {{"pubsub.topics.create", "name", true}},
	publisher + "UpdateTopic":              {{"pubsub.topics.update", "topic.name", false}},
	publisher + "Publish":                  {{"pubsub.topics.publish", "topic", false}},
	publisher + "GetTopic":                 {{"pubsub.topics.get", "topic", false}},
	publisher + "ListTopics":               {{"pubsub.topics.list", "project", true}},
	publisher + "ListTopicSubscriptions":   {{"pubsub.topics.get", "topic", false}},
	publisher + "ListTopicSnapshots":       {{"pubsub.topics.get", "topic", false}},
	publisher + "DeleteTopic":              {{"pubsub.topics.delete", "topic", false}},
	subscriber + "CreateSubscription":      {{"pubsub.subscriptions.create", "name", true}, {"pubsub.topics.attachSubscription", "topic", false}},
	subscriber + "GetSubscription":         {{"pubsub.subscriptions.get", "subscription", false}},
	subscriber + "UpdateSubscription":      {{"pubsub.subscriptions.update", "subscription.name", false}},
	subscriber + "ListSubscriptions":       {{"pubsub.subscriptions.list", "project", true}},
	subscriber + "DeleteSubscription":      {{"pubsub.subscriptions.delete", "subscription", false}},
	subscriber + "ModifyAckDeadline":       {{"pubsub.subscriptions.consume", "subscription", false}},
	subscriber + "Acknowledge":             {{"pubsub.subscriptions.consume", "subscription", false}},
	subscriber + "Pull":                    {{"pubsub.subscriptions.consume", "subscription", false}},
	subscriber + "StreamingPull":           {{"pubsub.subscriptions.consume", "subscription", false}},
	subscriber + "ModifyPushConfig":        {{"pubsub.subscriptions.update", "subscription", false}},
	subscriber + "GetSnapshot":             {{"pubsub.snapshots.get", "snapshot", false}},
	subscriber + "ListSnapshots":           {{"pubsub.snapshots.list", "project", true}},
	subscriber + "CreateSnapshot":          {{"pubsub.snapshots.create", "name", true}, {"pubsub.subscriptions.consume", "subscription", false}},
	subscriber + "UpdateSnapshot":          {{"pubsub.snapshots.update", "snapshot.name", false}},
	subscriber + "DeleteSnapshot":          {{"pubsub.snapshots.delete", "snapshot", false}},
	subscriber + "Seek":                    {{"pubsub.subscriptions.consume", "subscription", false}, {"pubsub.snapshots.seek", "snapshot", false}},
	publisher + "DetachSubscription":       {{"pubsub.topics.detachSubscription", "subscription", false}},
	schemaService + "CreateSchema":         {{"pubsub.schemas.create", "parent", true}},
	schemaService + "GetSchema":            {{"pubsub.schemas.get", "name", false}},
	schemaService + "ListSchemas":          {{"pubsub.schemas.list", "parent", true}},
	schemaService + "ListSchemaRevisions":  {{"pubsub.schemas.listRevisions", "name", false}},
	schemaService + "CommitSchema":         {{"pubsub.schemas.commit", "name", false}},
	schemaService + "RollbackSchema":       {{"pubsub.schemas.rollback", "name", false}},
	schemaService + "DeleteSchemaRevision": {{"pubsub.schemas.delete", "name", false}},
	schemaService + "DeleteSchema":         {{"pubsub.schemas.delete", "name", false}},
	schemaService + "ValidateSchema":       {{"pubsub.schemas.validate", "parent", true}},
	schemaService + "ValidateMessage":      {{"pubsub.schemas.validate", "parent", true}},
	iamService + "GetIamPolicy":            {{"getIamPolicy", "resource", false}},
	iamService + "SetIamPolicy":            {{"setIamPolicy", "resource", false}},
}

// psActAs are the methods that attach a push subscription's OIDC token
// service account, by the request field naming it: the caller needs
// iam.serviceAccounts.actAs on it (#1879), as "Authenticate push
// subscriptions" asks of whoever creates or updates such a subscription
// (https://docs.cloud.google.com/pubsub/docs/authenticate-push-subscriptions,
// read 2026-11-20). UpdateSubscription attaches it only when its update
// mask names the push config.
var psActAs = map[string]string{
	subscriber + "CreateSubscription": "push_config.oidc_token.service_account_email",
	subscriber + "UpdateSubscription": "subscription.push_config.oidc_token.service_account_email",
	subscriber + "ModifyPushConfig":   "push_config.oidc_token.service_account_email",
}

// pushConfigMasked is whether an UpdateSubscription request's mask names
// its push config (or a field in it).
func pushConfigMasked(m protoreflect.Message) bool {
	fd := m.Descriptor().Fields().ByName("update_mask")
	if fd == nil || !m.Has(fd) {
		return false
	}
	paths := m.Get(fd).Message()
	pd := paths.Descriptor().Fields().ByName("paths")
	if pd == nil {
		return false
	}
	l := paths.Get(pd).List()
	for i := 0; i < l.Len(); i++ {
		p := l.Get(i).String()
		if p == "push_config" || strings.HasPrefix(p, "push_config.") || p == "pushConfig" || strings.HasPrefix(p, "pushConfig.") {
			return true
		}
	}
	return false
}

// authorizeActAs checks the push subscription's OIDC token account a
// request attaches, nil when it names none or the caller may act as it.
func (f *Front) authorizeActAs(ctx context.Context, method string, m protoreflect.Message) error {
	path, ok := psActAs[method]
	if !ok {
		return nil
	}
	if method == subscriber+"UpdateSubscription" && !pushConfigMasked(m) {
		return nil
	}
	account := stringField(m, path)
	if account == "" {
		return nil
	}
	sub := stringField(m, "name")
	if sub == "" {
		sub = stringField(m, "subscription")
	}
	if sub == "" {
		sub = stringField(m, "subscription.name")
	}
	d, err := f.iamCheck.Check(ctx, iamcheck.Request{Bearer: bearerOf(ctx), Project: projectOf(sub), ActAs: account})
	if err != nil {
		return status.Errorf(codes.Unavailable, "IAM check unavailable: %v", err)
	}
	if d.Unauthenticated {
		return unauthenticated()
	}
	if !d.Holds("iam.serviceAccounts.actAs") {
		return denied()
	}
	return nil
}

// denied is Pub/Sub's refusal.
func denied() error {
	return status.Error(codes.PermissionDenied, "User not authorized to perform this action.")
}

func unauthenticated() error {
	return status.Error(codes.Unauthenticated, "Request had invalid authentication credentials. Expected OAuth 2 access token, "+
		"login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project.")
}

// UseIAMCheck makes the front check calls with c (nil: none).
func (f *Front) UseIAMCheck(c *iamcheck.Client) { f.iamCheck = c }

// requestOf decodes a method's first request frame.
func requestOf(method string, fr frame) protoreflect.Message {
	svc, name, ok := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	if !ok {
		return nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return nil
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
	if err != nil {
		return nil
	}
	m := mt.New()
	if proto.Unmarshal(fr, m.Interface()) != nil {
		return nil
	}
	return m
}

// stringField reads a dotted string field of m, "" when absent.
func stringField(m protoreflect.Message, path string) string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return ""
		}
		if i == len(parts)-1 {
			if fd.Kind() != protoreflect.StringKind {
				return ""
			}
			return m.Get(fd).String()
		}
		if fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
			return ""
		}
		m = m.Get(fd).Message()
	}
	return ""
}

func bearerOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t := iamcheck.Bearer(v); t != "" {
			return t
		}
	}
	return ""
}

// typeOf is resource.type of a Pub/Sub resource name.
func typeOf(name string) string {
	switch parts := strings.Split(name, "/"); {
	case len(parts) == 2:
		return "cloudresourcemanager.googleapis.com/Project"
	case len(parts) == 4 && parts[2] == "topics":
		return "pubsub.googleapis.com/Topic"
	case len(parts) == 4 && parts[2] == "subscriptions":
		return "pubsub.googleapis.com/Subscription"
	case len(parts) == 4 && parts[2] == "snapshots":
		return "pubsub.googleapis.com/Snapshot"
	case len(parts) == 4 && parts[2] == "schemas":
		return "pubsub.googleapis.com/Schema"
	}
	return ""
}

// checkRequest is the check of perms on resource, with its own policy.
func (f *Front) checkRequest(ctx context.Context, resource string, perms []string) iamcheck.Request {
	req := iamcheck.Request{Bearer: bearerOf(ctx), Service: "pubsub.googleapis.com", Permissions: perms,
		Resource: resource, Type: typeOf(resource), Project: projectOf(resource)}
	if strings.Count(resource, "/") == 3 {
		req.Own = []iamcheck.Policy{{Resource: "//pubsub.googleapis.com/" + resource, Policy: f.storedIAM(resource)}}
	}
	return req
}

// authorize checks a call's first request frame; nil when it may run.
func (f *Front) authorize(ctx context.Context, method string, fr frame) error {
	if !f.iamCheck.Enabled() {
		return nil
	}
	checks, ok := psChecks[method]
	if !ok || len(checks) == 0 {
		return nil
	}
	m := requestOf(method, fr)
	if m == nil {
		return nil
	}
	for _, c := range checks {
		name := stringField(m, c.field)
		if i := strings.IndexByte(name, '@'); i > 0 {
			// A schema revision, projects/p/schemas/s@rev: the schema's.
			name = name[:i]
		}
		if name == "" {
			continue
		}
		if onTopic[method] {
			if _, gone := f.detachedSub(name); gone {
				continue
			}
			sub, err := f.subscription(ctx, name)
			if err != nil {
				// The method answers NOT_FOUND itself.
				continue
			}
			name = sub.GetTopic()
		}
		perm := c.perm
		if !strings.Contains(perm, ".") {
			kind, err := iamResource(name)
			if err != nil {
				return nil
			}
			perm = "pubsub." + kind + "." + perm
		}
		if c.project {
			name = "projects/" + projectOf(name)
		}
		d, err := f.iamCheck.Check(ctx, f.checkRequest(ctx, name, []string{perm}))
		if err != nil {
			return status.Errorf(codes.Unavailable, "IAM check unavailable: %v", err)
		}
		if d.Unauthenticated {
			return unauthenticated()
		}
		if !d.Holds(perm) {
			return denied()
		}
	}
	return f.authorizeActAs(ctx, method, m)
}

// held are those of perms the caller holds on resource; every one when
// enforcement is off.
func (f *Front) held(ctx context.Context, resource string, perms []string) []string {
	if !f.iamCheck.Enabled() || len(perms) == 0 || bearerOf(ctx) == "" {
		return perms
	}
	d, err := f.iamCheck.Check(ctx, f.checkRequest(ctx, resource, perms))
	if err != nil || d.Unauthenticated {
		return nil
	}
	var out []string
	for _, p := range perms {
		if d.Holds(p) {
			out = append(out, p)
		}
	}
	return out
}

// enforce reads ss's first request and checks it, giving back a stream
// that replays it.
func (f *Front) enforce(ss grpc.ServerStream, method string) (grpc.ServerStream, error) {
	if !f.iamCheck.Enabled() {
		return ss, nil
	}
	if _, ok := psChecks[method]; !ok || bearerOf(ss.Context()) == "" {
		// No bearer token is the instance's own account, as the host
		// decides (iameval.CheckRemote).
		return ss, nil
	}
	var first frame
	if err := ss.RecvMsg(&first); err != nil {
		return nil, err
	}
	if err := f.authorize(ss.Context(), method, first); err != nil {
		return nil, err
	}
	return &replayStream{ServerStream: ss, first: first}, nil
}
