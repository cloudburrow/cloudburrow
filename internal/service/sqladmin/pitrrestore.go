package sqladmin

// instances.pointInTimeRestore (#1425): "Point in time restore for an
// instance managed by Google Cloud Backup and Disaster Recovery". Its
// datasource is a Backup and DR Service datasource,
// projects/{project}/locations/{region}/backupVaults/{vault}/dataSources/{datasource},
// which exists once a backup plan association enrolls an instance in a
// backup vault (internal/service/backupdr, #1433). The request is checked as
// Google checks it; the datasource is resolved to the instance it protects,
// and the target is made by instances.clone's path to a point in time
// (pitr.go). Without the Backup and DR Service the call is 501.

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

// datasourceRE is a Backup and DR datasource name.
var datasourceRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/backupVaults/[^/]+/dataSources/[^/]+$`)

// pointInTimeRestore serves POST projects/{project}:pointInTimeRestore,
// whose body is a PointInTimeRestoreContext.
func (s *Server) pointInTimeRestore(r *http.Request, base, project string) (any, error) {
	var body struct {
		Datasource             string `json:"datasource"`
		TargetInstance         string `json:"targetInstance"`
		PointInTime            string `json:"pointInTime"`
		PreferredZone          string `json:"preferredZone"`
		PreferredSecondaryZone string `json:"preferredSecondaryZone"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	switch {
	case body.Datasource == "":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.datasource is required.")
	case !datasourceRE.MatchString(body.Datasource):
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.datasource %q is not a Backup and DR "+
			"datasource: the format is projects/{project}/locations/{region}/backupVaults/{backupvault}/dataSources/{datasource}.",
			body.Datasource)
	case body.TargetInstance == "":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.targetInstance is required.")
	case !instanceNameRE.MatchString(body.TargetInstance) || len(body.TargetInstance) > maxInstanceName:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance name %q must be lowercase letters, digits and hyphens, "+
			"start with a letter, not end with a hyphen, and be at most %d characters in CloudBurrow.", body.TargetInstance, maxInstanceName)
	case body.PointInTime == "":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.pointInTime is required.")
	case body.PreferredZone != "" && strings.EqualFold(body.PreferredZone, body.PreferredSecondaryZone):
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.preferredSecondaryZone cannot be the "+
			"same as preferredZone.")
	}
	if _, err := time.Parse(time.RFC3339Nano, body.PointInTime); err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: pointInTimeRestoreContext.pointInTime %q is not an RFC 3339 time.",
			body.PointInTime)
	}
	bdr := s.backupDR()
	if bdr == nil {
		return nil, unimplemented("instances.pointInTimeRestore from "+body.Datasource, "the Backup and DR Service "+
			"(backupdr.googleapis.com) is not served by this instance, so no instance is managed by it; start it with "+
			"--services backupdr (docs/backupdr.md), or recover to a point in time with instances.clone and "+
			"cloneContext.pointInTime (docs/cloudsql.md)")
	}
	srcProject, instance, err := bdr.DataSourceInstance(body.Datasource)
	if err != nil {
		return nil, Errorf(http.StatusNotFound, "%s", err.Error())
	}
	if srcProject != project {
		return nil, unimplemented("instances.pointInTimeRestore into another project",
			"the target is created in the datasource's instance's project, "+srcProject+"; call it with parent projects/"+srcProject)
	}
	e := s.engine(instance)
	if e == nil || (s.createdNamed(project, instance) == nil && s.engineIsCreated(instance)) {
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL instance %s the datasource %s protects no longer exists; restore one of "+
			"its backups with instances.restoreBackup and backupdrBackup.", instance, body.Datasource)
	}
	// The clone-to-a-time path (#1330): a new instance with the source's
	// settings, recovered to pointInTime from its bases and its log.
	return s.cloneInstance(cloneRequest(body.TargetInstance, body.PointInTime), base, project, e)
}

// engineIsCreated is whether instance is a created instance (of any project).
func (s *Server) engineIsCreated(instance string) bool {
	s.emu.RLock()
	defer s.emu.RUnlock()
	return s.writes != nil && s.writes.byName[instance] != nil
}
