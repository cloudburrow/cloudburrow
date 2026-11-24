package compute

// lbUnserved are the load-balancing methods that answer 501 (#2153, #2160), at
// their discovery document's paths.
var lbUnserved = []struct{ id, method, path string }{
	{"compute.sslPolicies.getIamPolicy", "GET", "projects/p/global/sslPolicies/x/getIamPolicy"},
	{"compute.sslPolicies.setIamPolicy", "POST", "projects/p/global/sslPolicies/x/setIamPolicy"},
	{"compute.regionSslPolicies.getIamPolicy", "GET", "projects/p/regions/us-central1/sslPolicies/x/getIamPolicy"},
	{"compute.regionSslPolicies.setIamPolicy", "POST", "projects/p/regions/us-central1/sslPolicies/x/setIamPolicy"},
}
