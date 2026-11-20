package datatransfer

import (
	_ "embed"
	"encoding/json"
)

// discoveryV1 is Google's discovery document of
// bigquerydatatransfer.googleapis.com v1, as google.golang.org/api v0.314.0
// ships it (bigquerydatatransfer/v1/bigquerydatatransfer-api.json). bq
// builds its Data Transfer client from it: it asks the address --api names
// for $discovery/rest?version=v1, the same address it sends BigQuery's
// calls to (#1948).
//
//go:embed discovery_v1.json
var discoveryV1 []byte

// DiscoveryDocument is the v1 discovery document with its rootUrl and
// mtlsRootUrl set to root (http://host:port/), so a client built from it
// calls this instance.
func DiscoveryDocument(root string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(discoveryV1, &doc); err != nil {
		return nil, err
	}
	doc["rootUrl"] = root
	doc["mtlsRootUrl"] = root
	doc["baseUrl"] = root
	return json.Marshal(doc)
}
