package main

import (
	"net/http/httptest"
	"testing"
)

// TestDataTransferLeavesTheConnectionAPIToBigQuery: the BigQuery port passes
// /v1/ to the Data Transfer Service (#1948), except the BigQuery Connection
// API's paths (#1755), which the BigQuery front serves on the same port.
func TestDataTransferLeavesTheConnectionAPIToBigQuery(t *testing.T) {
	s := &dataTransferSvc{}
	addr := "127.0.0.1:1"
	s.bound.Store(&addr)
	for path, want := range map[string]bool{
		"/v1/projects/p/locations/us/transferConfigs":              true,
		"/v1/projects/p/transferConfigs/c/runs":                    true,
		"/v1/projects/p/locations/us/dataSources":                  true,
		"/v1/projects/p/locations":                                 true,
		"/v1/projects/p/locations/us/connections":                  false,
		"/v1/projects/p/locations/us/connections/c":                false,
		"/v1/projects/p/locations/us/connections/c:getIamPolicy":   false,
		"/v1/projects/p/locations/us/connectionsX/transferConfigs": true,
		"/bigquery/v2/projects/p/datasets":                         false,
	} {
		got := s.bigQueryLocal(httptest.NewRequest("GET", path, nil)) != nil
		if got != want {
			t.Errorf("%s: to Data Transfer = %v, want %v", path, got, want)
		}
	}
}
