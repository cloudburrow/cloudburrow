package run

import (
	"strings"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// eaAttr is one filtering attribute of an event type, as ListProviders
// describes it.
type eaAttr struct {
	name, description string
	required, pattern bool
}

// eaTypeAttrs are the filtering attributes, other than type, of each
// event type served, as `gcloud eventarc providers describe` lists them.
var eaTypeAttrs = map[string][]eaAttr{
	pubsubPublished:                        nil,
	storageEventPrefix + "finalized":       {{name: "bucket", description: "The bucket's name", required: true}},
	storageEventPrefix + "deleted":         {{name: "bucket", description: "The bucket's name", required: true}},
	storageEventPrefix + "archived":        {{name: "bucket", description: "The bucket's name", required: true}},
	storageEventPrefix + "metadataUpdated": {{name: "bucket", description: "The bucket's name", required: true}},
	auditLogWritten: {
		{name: "serviceName", description: "The identifier of the service that wrote the audit log entry", required: true},
		{name: "methodName", description: "The identifier of the method that wrote the audit log entry", required: true},
		{name: "resourceName", description: "The resource or collection that is the target of the operation", pattern: true},
	},
}

// sourcedEventType reports an event type Eventarc sources itself, through
// a topic it makes and publishes to: audit logs and Firestore.
func sourcedEventType(typ string) bool {
	_, served := eaTypeAttrs[typ]
	return served && typ != pubsubPublished && storageNotificationTypes[typ] == ""
}

// eaContentType is the event data's content type: Firestore's is
// protobuf, as Google's Firestore triggers document.
func eaContentType(typ string) string {
	if isFirestoreType(typ) {
		return "application/protobuf"
	}
	return "application/json"
}

// eaCheckFilters refuses filters the event type does not take: an unknown
// attribute, a missing required one, a repeated one, or a path pattern on
// an attribute that does not support one, or an invalid pattern.
func eaCheckFilters(spec *functionspb.EventTrigger) error {
	typ := spec.GetEventType()
	if typ == "" {
		return apierror.InvalidArgument("the trigger's event filters must name its event type: attribute \"type\"")
	}
	attrs, ok := eaTypeAttrs[typ]
	if !ok {
		return apierror.Unimplemented("event type %q is not supported; %s", typ, eventReason(typ))
	}
	seen := map[string]bool{}
	for _, f := range spec.GetEventFilters() {
		var def *eaAttr
		for i := range attrs {
			if attrs[i].name == f.GetAttribute() {
				def = &attrs[i]
			}
		}
		if def == nil {
			return apierror.InvalidArgument("event filter attribute %q is not one of %s's filtering attributes", f.GetAttribute(), typ)
		}
		if seen[f.GetAttribute()] {
			return apierror.InvalidArgument("event filter attribute %q is given more than once", f.GetAttribute())
		}
		seen[f.GetAttribute()] = true
		if f.GetOperator() == pathPatternOp {
			if !def.pattern {
				return apierror.InvalidArgument("event filter attribute %q of %s does not support path patterns", f.GetAttribute(), typ)
			}
			if err := validPathPattern(f.GetValue()); err != nil {
				return apierror.InvalidArgument("%v", err)
			}
		}
		if f.GetValue() == "" {
			return apierror.InvalidArgument("event filter attribute %q has no value", f.GetAttribute())
		}
	}
	for _, a := range attrs {
		if a.required && !seen[a.name] {
			return apierror.InvalidArgument("%s's event filters must include the attribute %q", typ, a.name)
		}
	}
	return nil
}

func isFirestoreType(typ string) bool {
	return strings.HasPrefix(typ, "google.cloud.firestore.document.v1.")
}
