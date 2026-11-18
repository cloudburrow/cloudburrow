package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConnectionsRewriteMember(t *testing.T) {
	sa, del := "serviceAccount:a@p.iam.gserviceaccount.com", "deleted:serviceAccount:a@p.iam.gserviceaccount.com?uid=1"
	pol := `{"version":3,"bindings":[{"role":"roles/bigquery.connectionUser","members":["` + sa + `","user:u@x.com"],` +
		`"condition":{"title":"t","expression":"true"}},{"role":"roles/bigquery.connectionAdmin","members":["` + sa + `"]},` +
		`{"role":"roles/viewer","members":["user:u@x.com"]}]}`
	c := &bqConnections{conns: map[string]*bqConnection{"k": {Project: "1", Location: "us", ID: "c", Policy: json.RawMessage(pol)},
		"other": {Project: "1", Location: "us", ID: "d"}}}
	get := func() string { return string(c.conns["k"].Policy) }

	_ = c.rewriteMember(sa, del)
	if p := get(); strings.Contains(p, `"`+sa+`"`) || strings.Count(p, del) != 2 || !strings.Contains(p, `"title":"t"`) || c.conns["k"].Etag != 1 {
		t.Fatalf("delete: %s", p)
	}
	_ = c.rewriteMember(del, sa) // undelete
	if p := get(); strings.Contains(p, del) || strings.Count(p, `"`+sa+`"`) != 2 {
		t.Fatalf("undelete: %s", p)
	}
	_ = c.rewriteMember(sa, del)
	_ = c.rewriteMember(del, "") // purge
	p := get()
	if strings.Contains(p, del) || strings.Contains(p, "connectionAdmin") || !strings.Contains(p, "connectionUser") || !strings.Contains(p, "roles/viewer") {
		t.Fatalf("purge: %s", p)
	}
	etag := c.conns["k"].Etag
	_ = c.rewriteMember("serviceAccount:none@p.iam.gserviceaccount.com", "")
	if c.conns["k"].Etag != etag || c.conns["other"].Policy != nil {
		t.Errorf("an unnamed member changed a policy")
	}
}
