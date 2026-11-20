package iameval

import "testing"

func TestLoggingExtraJSON(t *testing.T) {
	for _, c := range []struct{ method, path, perm, res string }{
		{"GET", "/v2/projects/p/locations/global/savedQueries", "logging.queries.list", "projects/p/locations/global"},
		{"POST", "/v2/folders/1/locations/global/savedQueries", "logging.queries.create", "folders/1/locations/global"},
		{"GET", "/v2/projects/p/locations/global/savedQueries/q", "logging.queries.get", "projects/p/locations/global/savedQueries/q"},
		{"PATCH", "/v2/projects/p/locations/global/savedQueries/q", "logging.queries.update", "projects/p/locations/global/savedQueries/q"},
		{"DELETE", "/v2/projects/p/locations/global/logScopes/s", "logging.logScopes.delete", "projects/p/locations/global/logScopes/s"},
		{"GET", "/v2/projects/p/locations/-/logScopes", "logging.logScopes.list", "projects/p/locations/-"},
	} {
		perm, res, ok := MatchJSON(LoggingExtraJSON, c.method, c.path)
		if !ok || perm != c.perm || res != c.res {
			t.Errorf("%s %s = %q %q %v; want %q %q", c.method, c.path, perm, res, ok, c.perm, c.res)
		}
	}
	if _, ok := Logging.Methods["/google.logging.v2.LoggingServiceV2/TailLogEntries"]; !ok {
		t.Error("TailLogEntries is not in Logging's table")
	}
}

func TestMemorystoreOperation(t *testing.T) {
	for _, c := range []struct {
		typeURL string
		valkey  bool
		svc     *Service
		get     string
	}{
		{"type.googleapis.com/google.cloud.redis.v1.OperationMetadata", false, Redis, "redis.operations.get"},
		{"type.googleapis.com/google.cloud.redis.cluster.v1.OperationMetadata", false, RedisCluster, "redis.operations.get"},
		{"type.googleapis.com/google.cloud.memorystore.v1.OperationMetadata", false, Valkey, "memorystore.operations.get"},
		{"", true, Valkey, "memorystore.operations.get"},
		{"type.googleapis.com/google.cloud.other.v1.OperationMetadata", false, nil, ""},
	} {
		svc, get, _, _, _ := MemorystoreOperation(c.typeURL, c.valkey)
		if svc != c.svc || get != c.get {
			t.Errorf("MemorystoreOperation(%q, %v) = %v %q; want %v %q", c.typeURL, c.valkey, svc, get, c.svc, c.get)
		}
	}
	if svc, get, _, _ := RunPortOperation("type.googleapis.com/google.cloud.eventarc.v1.OperationMetadata"); svc != Eventarc || get != "eventarc.operations.get" {
		t.Errorf("RunPortOperation(eventarc) = %v %q", svc, get)
	}
}

func TestDenyCustomerPrincipalSet(t *testing.T) {
	DomainCustomers["example.com"] = "C0abc"
	defer delete(DomainCustomers, "example.com")
	ids := denyIdentifiers(map[string]bool{"user:a@example.com": true, "domain:example.com": true})
	if !ids["principalset://goog/cloudidentitycustomerid/c0abc"] {
		t.Errorf("identifiers %v lack the customer's principal set", ids)
	}
	if matchIdentifier([]string{"principalSet://goog/cloudIdentityCustomerId/C0abc"}, ids) == "" {
		t.Error("the customer's principal set does not match its domain's user")
	}
	if denyIdentifiers(map[string]bool{"user:a@other.com": true})["principalset://goog/cloudidentitycustomerid/c0abc"] {
		t.Error("a user outside the domain matched the customer")
	}
}
