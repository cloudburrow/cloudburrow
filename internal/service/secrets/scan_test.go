package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// listingRunner answers a kubectl list with Secrets it holds, and records
// every call.
type listingRunner struct {
	items []map[string]any
	calls []string
}

func (r *listingRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	b, _ := json.Marshal(map[string]any{"items": r.items})
	return string(b), nil
}

// ListSecrets and ListVersions over Kubernetes read with the one kubectl
// list, not a kubectl get per secret or version, while they hold the Store's
// lock: with a few hundred records, a get each kept every other request
// waiting (#1232).
func TestListingsAreOneKubectlList(t *testing.T) {
	r := &listingRunner{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("s%d", i)
		sec, _ := json.Marshal(Secret{Name: SecretName("p", id)})
		ver, _ := json.Marshal(Version{Name: VersionName("p", id, 1), Number: 1, State: StateEnabled})
		r.items = append(r.items, map[string]any{
			"metadata": map[string]any{"name": KubernetesSecretName("p", id), "annotations": map[string]string{
				SecretAnnotation: string(sec), VersionAnnotationPrefix + "1": string(ver),
				SecretIDAnnotation: id, "cloudburrow.dev/secret-project": "p",
			}},
			"data": map[string]string{VersionKey(1): base64.StdEncoding.EncodeToString([]byte("payload"))},
		})
	}
	st := NewStore(NewKubeStore(k8s.NewWith(r, "", "", "default"), "inst"))
	all, err := st.ListSecrets("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 30 || len(r.calls) != 1 || !strings.Contains(r.calls[0], "get secret -l") {
		t.Errorf("ListSecrets returned %d secrets with %d kubectl calls %q; want 30 with one list", len(all), len(r.calls), r.calls)
	}

	r.calls = nil
	versions, err := st.listVersions("p", "s7")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || string(versions[0].Payload) != "payload" || len(r.calls) != 1 {
		t.Errorf("listVersions returned %v with %d kubectl calls; want version 1 and its payload with one list", versions, len(r.calls))
	}
}
