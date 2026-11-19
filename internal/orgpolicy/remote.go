package orgpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// Path is the host's effective-policy endpoint, on the Resource Manager
// port, which the servers CloudBurrow runs in the cluster (Cloud Storage,
// Pub/Sub's front) ask through Remote. `up` always serves it, and always
// gives them its address (--resourcemanager), whatever --iam-enforcement.
const Path = "/cloudburrow/v1/orgpolicy:effective"

// Request asks for constraint's effective policy on resource.
type Request struct {
	Resource   string `json:"resource"`
	Constraint string `json:"constraint"`
}

// Response is the answer: the specs as protojson, the tags as is.
type Response struct {
	Spec   json.RawMessage `json:"spec,omitempty"`
	DryRun json.RawMessage `json:"dryRun,omitempty"`
	Tags   []iamcond.Tag   `json:"tags,omitempty"`
}

// Remote asks the host at Base ("http://host:port").
type Remote struct {
	Base   string
	Client *http.Client
}

func (c Remote) Effective(ctx context.Context, resource, constraint string) (Effective, error) {
	body, _ := json.Marshal(Request{Resource: resource, Constraint: constraint})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+Path, bytes.NewReader(body))
	if err != nil {
		return Effective{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Effective{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Effective{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Effective{}, fmt.Errorf("orgpolicy: %s: %s", resp.Status, raw)
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Effective{}, err
	}
	return out.effective()
}

// effective decodes the answer.
func (out Response) effective() (Effective, error) {
	e := Effective{Tags: out.Tags}
	if len(out.Spec) > 0 {
		e.Spec = &orgpolicypb.PolicySpec{}
		if err := protojson.Unmarshal(out.Spec, e.Spec); err != nil {
			return Effective{}, err
		}
	}
	if len(out.DryRun) > 0 {
		e.DryRun = &orgpolicypb.PolicySpec{}
		if err := protojson.Unmarshal(out.DryRun, e.DryRun); err != nil {
			return Effective{}, err
		}
	}
	return e, nil
}

// AsResponse is e encoded as Path answers it.
func AsResponse(e Effective) Response {
	out := Response{Tags: e.Tags}
	if e.Spec != nil {
		out.Spec, _ = protojson.Marshal(e.Spec)
	}
	if e.DryRun != nil {
		out.DryRun, _ = protojson.Marshal(e.DryRun)
	}
	return out
}

// Handler serves Path from s.
func Handler(s Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var in Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e, err := s.Effective(r.Context(), in.Resource, in.Constraint)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		out := AsResponse(e)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}
