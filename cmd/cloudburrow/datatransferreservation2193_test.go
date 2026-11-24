package main

import "testing"

// TestReservationAPIPathIsTheFronts (#2193): the BigQuery port's Reservation
// API paths go to the BigQuery front, Data Transfer's to Data Transfer.
func TestReservationAPIPathIsTheFronts(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/projects/p/locations/US/reservations":                      true,
		"/v1/projects/p/locations/US/reservations/r/assignments/a:move": true,
		"/v1/projects/p/locations/US/capacityCommitments/c:split":       true,
		"/v1/projects/p/locations/US/capacityCommitments:merge":         true,
		"/v1/projects/p/locations/US/reservationGroups":                 true,
		"/v1/projects/p/locations/US/biReservation":                     true,
		"/v1/projects/p/locations/US:searchAssignments":                 true,
		"/v1/projects/p/locations/US:searchAllAssignments":              true,
		"/v1/projects/p/locations/US:upgradeEdition":                    true,
		"/v1/projects/p/locations/US/transferConfigs":                   false,
		"/v1/projects/p/locations/US":                                   false,
		"/v1/projects/p/locations/us/dataSources/scheduled_query":       false,
		"/v1/projects/p/locations/US/transferConfigs/c:startManualRuns": false,
		"/v1/projects/p/locations/US:enrollDataSources":                 false,
	} {
		if got := reservationAPIPath.MatchString(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}
