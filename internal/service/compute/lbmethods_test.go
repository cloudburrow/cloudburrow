package compute

// lbUnserved are the load-balancing methods that answer 501 (#2153), at
// their discovery document's paths.
var lbUnserved = []struct{ id, method, path string }{
	{"compute.backendBuckets.addSignedUrlKey", "POST", "projects/p/global/backendBuckets/x/addSignedUrlKey"},
	{"compute.backendBuckets.deleteSignedUrlKey", "POST", "projects/p/global/backendBuckets/x/deleteSignedUrlKey"},
	{"compute.backendBuckets.setEdgeSecurityPolicy", "POST", "projects/p/global/backendBuckets/x/setEdgeSecurityPolicy"},
	{"compute.backendServices.addSignedUrlKey", "POST", "projects/p/global/backendServices/x/addSignedUrlKey"},
	{"compute.backendServices.deleteSignedUrlKey", "POST", "projects/p/global/backendServices/x/deleteSignedUrlKey"},
	{"compute.backendServices.getEffectiveSecurityPolicies", "GET", "projects/p/global/backendServices/x/getEffectiveSecurityPolicies"},
	{"compute.backendServices.setEdgeSecurityPolicy", "POST", "projects/p/global/backendServices/x/setEdgeSecurityPolicy"},
	{"compute.backendServices.setSecurityPolicy", "POST", "projects/p/global/backendServices/x/setSecurityPolicy"},
	{"compute.regionBackendServices.setSecurityPolicy", "POST", "projects/p/regions/us-central1/backendServices/x/setSecurityPolicy"},
	{"compute.targetHttpsProxies.setCertificateMap", "POST", "projects/p/global/targetHttpsProxies/x/setCertificateMap"},
	{"compute.targetSslProxies.setCertificateMap", "POST", "projects/p/global/targetSslProxies/x/setCertificateMap"},
	{"compute.targetPools.setSecurityPolicy", "POST", "projects/p/regions/us-central1/targetPools/x/setSecurityPolicy"},
	{"compute.targetInstances.setSecurityPolicy", "POST", "projects/p/zones/us-central1-a/targetInstances/x/setSecurityPolicy"},
	{"compute.sslPolicies.getIamPolicy", "GET", "projects/p/global/sslPolicies/x/getIamPolicy"},
	{"compute.sslPolicies.setIamPolicy", "POST", "projects/p/global/sslPolicies/x/setIamPolicy"},
	{"compute.regionSslPolicies.getIamPolicy", "GET", "projects/p/regions/us-central1/sslPolicies/x/getIamPolicy"},
	{"compute.regionSslPolicies.setIamPolicy", "POST", "projects/p/regions/us-central1/sslPolicies/x/setIamPolicy"},
}
