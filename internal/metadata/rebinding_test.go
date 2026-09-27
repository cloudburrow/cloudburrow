package metadata

import (
	"net/http"
	"testing"
)

// The metadata server hands out tokens and the project's identity, so a
// rebound page must not read it (#676). Metadata-Flavor does not stop one:
// a same-origin page may set any header. Pods and the host keep working.
func TestMetadataRefusesForeignHosts(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)
	for host, want := range map[string]int{
		"attacker.example:9004": http.StatusMisdirectedRequest,
		"127.0.0.1:9004":        http.StatusOK,
		"cloudburrow-host.cloudburrow.svc.cluster.local:9004": http.StatusOK,
		"host.docker.internal:9004":                           http.StatusOK,
		"metadata.google.internal":                            http.StatusOK,
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/computeMetadata/v1/project/project-id", nil)
		req.Host = host
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("project-id with Host %s = %d, want %d", host, resp.StatusCode, want)
		}
	}
}
