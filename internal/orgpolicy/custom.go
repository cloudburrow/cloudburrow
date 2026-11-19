package orgpolicy

// Custom constraints (#1891): an organization's
// organizations/N/customConstraints/custom.{name}, a CEL condition over a
// service's resource, enforced as a boolean constraint is, by a policy
// that enforces custom.{name} on the resource's project or above it
// ("Creating and managing custom constraints", read 2026-11-19):
//
//   - "DENY: The operation is blocked when the condition evaluates to
//     true"; "ALLOW: permitted only if the condition evaluates to true";
//   - the condition is checked on the methodTypes the constraint names,
//     CREATE or UPDATE, against the resource as the request would leave
//     it ("changes to a violating resource are blocked unless the change
//     resolves the violation");
//   - a condition that cannot be evaluated (a missing list index or map
//     key) "blocks any attempt to create the resource";
//   - a dry-run spec logs, never refuses;
//   - custom constraints are checked before the predefined ones.
//
// The resource is the service's REST representation, by its JSON field
// names (resource.storageClass, resource.iamConfiguration...), which the
// caller hands over as a JSON-encodable value.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The resource types CloudBurrow enforces custom constraints on, each as
// Google documents it.
const (
	TypeBucket            = "storage.googleapis.com/Bucket"
	TypeServiceAccount    = "iam.googleapis.com/ServiceAccount"
	TypeServiceAccountKey = "iam.googleapis.com/ServiceAccountKey"
	TypeTopic             = "pubsub.googleapis.com/Topic"
	TypeSubscription      = "pubsub.googleapis.com/Subscription"
	TypeSecret            = "secretmanager.googleapis.com/Secret"
)

// Method types.
const (
	MethodCreate = "CREATE"
	MethodUpdate = "UPDATE"
)

// CustomPath is the host's endpoint for the custom constraints that apply
// to a resource type and method, beside Path.
const CustomPath = "/cloudburrow/v1/orgpolicy:custom"

// CustomConstraint is one custom constraint and its effective policy on
// the resource asked about.
type CustomConstraint struct {
	// Name is the short name, custom.{name}.
	Name        string `json:"name"`
	Condition   string `json:"condition"`
	ActionType  string `json:"actionType"` // ALLOW or DENY
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	// Effective is the policy of custom.{name} on the resource.
	Effective Response `json:"effective"`
}

// CustomSource answers the custom constraints of resourceType and method
// with their effective policies on resource; a Source that is not one
// enforces none.
type CustomSource interface {
	Customs(ctx context.Context, resource, resourceType, method string) ([]CustomConstraint, error)
}

// CustomRequest asks CustomPath.
type CustomRequest struct {
	Resource     string `json:"resource"`
	ResourceType string `json:"resourceType"`
	Method       string `json:"method"`
}

// Customs asks the host.
func (c Remote) Customs(ctx context.Context, resource, resourceType, method string) ([]CustomConstraint, error) {
	body, _ := json.Marshal(CustomRequest{Resource: resource, ResourceType: resourceType, Method: method})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+CustomPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("orgpolicy: %s: %s", resp.Status, raw)
	}
	var out []CustomConstraint
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CustomHandler serves CustomPath from s.
func CustomHandler(s CustomSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var in CustomRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := s.Customs(r.Context(), in.Resource, in.ResourceType, in.Method)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if out == nil {
			out = []CustomConstraint{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// CustomViolation is a request refused by custom constraints: each one's
// name (custom.{name}) and the message it shows, its description (its
// display name when it has none).
type CustomViolation struct {
	Names    []string
	Messages []string
}

// Error is Google's: `Operation denied by custom org policies:
// ["customConstraints/custom.a":"description"]` (the Secret Manager
// page's example, verbatim; the Pub/Sub and Cloud Run pages' begin the
// same).
func (v *CustomViolation) Error() string {
	return v.message("Operation denied by custom org policies: ")
}

// Singular is the IAM page's wording, "Operation denied by custom org
// policy: [...]".
func (v *CustomViolation) Singular() string {
	return v.message("Operation denied by custom org policy: ")
}

func (v *CustomViolation) message(prefix string) string {
	parts := make([]string, len(v.Names))
	for i, n := range v.Names {
		parts[i] = fmt.Sprintf("%q:%q", "customConstraints/"+n, v.Messages[i])
	}
	return prefix + "[" + strings.Join(parts, ", ") + "]"
}

// Bucket is Cloud Storage's 412 message for the first constraint,
// "orgpolicy:projects/_/buckets/B violates customConstraints/custom.x."
// (Cloud Storage's custom constraints page), and its details, the
// constraint's message.
func (v *CustomViolation) Bucket(bucket string) (string, string) {
	return fmt.Sprintf("orgpolicy:projects/_/buckets/%s violates customConstraints/%s.", bucket, v.Names[0]), v.Messages[0]
}

var (
	celOnce sync.Once
	celEnv  *cel.Env
	celErr  error
	progsMu sync.Mutex
	progs   = map[string]cel.Program{}
)

func env() (*cel.Env, error) {
	celOnce.Do(func() {
		celEnv, celErr = cel.NewEnv(cel.Variable("resource", cel.DynType), cel.CrossTypeNumericComparisons(true))
	})
	return celEnv, celErr
}

// CheckCondition reports why condition is not a CEL boolean expression
// over resource, or nil.
func CheckCondition(condition string) error {
	_, err := program(condition)
	return err
}

func program(condition string) (cel.Program, error) {
	progsMu.Lock()
	p, ok := progs[condition]
	progsMu.Unlock()
	if ok {
		return p, nil
	}
	e, err := env()
	if err != nil {
		return nil, err
	}
	ast, iss := e.Compile(condition)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if t := ast.OutputType(); t != cel.BoolType && t != cel.DynType {
		return nil, fmt.Errorf("the condition is of type %s, not bool", t)
	}
	p, err = e.Program(ast)
	if err != nil {
		return nil, err
	}
	progsMu.Lock()
	progs[condition] = p
	progsMu.Unlock()
	return p, nil
}

// typedFields are the fields of each resource type whose JSON string is
// a google.protobuf.Duration or Timestamp, which conditions compare with
// duration('30h') and timestamp('...') (the Secret Manager page's
// "resource.versionDestroyTtl < duration('30h')").
var typedFields = map[string]map[string]string{
	TypeSecret: {"expireTime": "timestamp", "rotation.nextRotationTime": "timestamp",
		"rotation.rotationPeriod": "duration", "versionDestroyTtl": "duration"},
	TypeTopic: {"messageRetentionDuration": "duration"},
	TypeSubscription: {"messageRetentionDuration": "duration", "expirationPolicy.ttl": "duration",
		"retryPolicy.minimumBackoff": "duration", "retryPolicy.maximumBackoff": "duration",
		"cloudStorageConfig.maxDuration": "duration"},
}

// normalize turns JSON numbers into int64 (when whole) or float64.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// typed converts resourceType's duration and timestamp fields of obj.
func typed(resourceType string, obj map[string]any) {
	for path, kind := range typedFields[resourceType] {
		parts := strings.Split(path, ".")
		m := obj
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				m = nil
				break
			}
			m = next
		}
		last := parts[len(parts)-1]
		str, ok := m[last].(string)
		if m == nil || !ok {
			continue
		}
		switch kind {
		case "duration":
			if d, err := time.ParseDuration(str); err == nil {
				m[last] = durationpb.New(d)
			}
		case "timestamp":
			if t, err := time.Parse(time.RFC3339Nano, str); err == nil {
				m[last] = timestamppb.New(t)
			}
		}
	}
}

// Evaluate is condition on resource (a JSON-encodable value): its
// verdict, or why it cannot be evaluated.
func Evaluate(condition string, resource any) (bool, error) {
	return evaluate(condition, "", resource)
}

func evaluate(condition, resourceType string, resource any) (bool, error) {
	p, err := program(condition)
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(resource)
	if err != nil {
		return false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return false, err
	}
	obj, _ := normalize(decoded).(map[string]any)
	if obj == nil {
		obj = map[string]any{}
	}
	typed(resourceType, obj)
	out, _, err := p.Eval(map[string]any{"resource": obj})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("the condition gave %v, not a bool", out.Value())
	}
	return b, nil
}

// violates reports whether resource violates c: DENY and true, ALLOW and
// not true (false, or not evaluable).
func violates(c CustomConstraint, resourceType string, resource any) bool {
	ok, err := evaluate(c.Condition, resourceType, resource)
	if err != nil {
		return true
	}
	if c.ActionType == "DENY" {
		return ok
	}
	return !ok
}

// CheckCustom refuses method (CREATE or UPDATE) leaving a resource of
// resourceType, under project or folder `under` ("projects/p"), as
// resource, when a custom constraint enforced there is violated: a
// *CustomViolation naming every one. nil when none applies or the
// hierarchy cannot be read.
func CheckCustom(ctx context.Context, under, resourceType, method string, resource any) error {
	cs, ok := current().(CustomSource)
	if !ok || under == "" {
		return nil
	}
	list, err := cs.Customs(ctx, under, resourceType, method)
	if err != nil || len(list) == 0 {
		return nil
	}
	var v CustomViolation
	for _, c := range list {
		e, err := c.Effective.effective()
		if err != nil {
			continue
		}
		if e.DryRun != nil && BoolEnforced(e.DryRun, e.Tags) && violates(c, resourceType, resource) {
			log.Printf("orgpolicy: dry run: the request on %s would violate customConstraints/%s", under, c.Name)
		}
		if !BoolEnforced(e.Spec, e.Tags) || !violates(c, resourceType, resource) {
			continue
		}
		msg := c.Description
		if msg == "" {
			msg = c.DisplayName
		}
		v.Names = append(v.Names, c.Name)
		v.Messages = append(v.Messages, msg)
	}
	if len(v.Names) == 0 {
		return nil
	}
	return &v
}
