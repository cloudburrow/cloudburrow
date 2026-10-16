package sqladmin

// instances.pointInTimeRestore (#1425): "Point in time restore for an
// instance managed by Google Cloud Backup and Disaster Recovery". Its
// datasource is a Backup and DR Service datasource,
// projects/{project}/locations/{region}/backupVaults/{vault}/dataSources/{datasource},
// which exists only once a backup plan association enrolls an instance in
// a backup vault. CloudBurrow does not serve the Backup and DR Service
// (backupdr.googleapis.com, #1433), so no instance is managed by it and no
// datasource can name one: the request is checked as Google checks it, and a
// well-formed one is refused with that reason. Point-in-time recovery itself
// is instances.clone with cloneContext.pointInTime (pitr.go).

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

// datasourceRE is a Backup and DR datasource name.
var datasourceRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/backupVaults/[^/]+/dataSources/[^/]+$`)

// pointInTimeRestoreWhy is why a well-formed pointInTimeRestore is refused.
const pointInTimeRestoreWhy = "it restores an instance managed by the Backup and DR Service from its datasource, and " +
	"CloudBurrow does not serve the Backup and DR Service (backupdr.googleapis.com), so no instance is managed by it " +
	"and no datasource exists (#1433); recover to a point in time with instances.clone and cloneContext.pointInTime " +
	"(docs/cloudsql.md)"

// pointInTimeRestore serves POST projects/{project}:pointInTimeRestore,
// whose body is a PointInTimeRestoreContext.
func (s *Server) pointInTimeRestore(r *http.Request) error {
	var body struct {
		Datasource             string `json:"datasource"`
		TargetInstance         string `json:"targetInstance"`
		PointInTime            string `json:"pointInTime"`
		PreferredZone          string `json:"preferredZone"`
		PreferredSecondaryZone string `json:"preferredSecondaryZone"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	switch {
	case body.Datasource == "":
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.datasource is required.")
	case !datasourceRE.MatchString(body.Datasource):
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.datasource %q is not a Backup and DR "+
			"datasource: the format is projects/{project}/locations/{region}/backupVaults/{backupvault}/dataSources/{datasource}.",
			body.Datasource)
	case body.TargetInstance == "":
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.targetInstance is required.")
	case !instanceNameRE.MatchString(body.TargetInstance) || len(body.TargetInstance) > maxInstanceName:
		return Errorf(http.StatusBadRequest, "Invalid request: instance name %q must be lowercase letters, digits and hyphens, "+
			"start with a letter, not end with a hyphen, and be at most %d characters in CloudBurrow.", body.TargetInstance, maxInstanceName)
	case body.PointInTime == "":
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.pointInTime is required.")
	case body.PreferredZone != "" && strings.EqualFold(body.PreferredZone, body.PreferredSecondaryZone):
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.preferredSecondaryZone cannot be the "+
			"same as preferredZone.")
	}
	if _, err := time.Parse(time.RFC3339Nano, body.PointInTime); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.pointInTime %q is not an RFC 3339 time.",
			body.PointInTime)
	}
	return unimplemented("instances.pointInTimeRestore from "+body.Datasource, pointInTimeRestoreWhy)
}
