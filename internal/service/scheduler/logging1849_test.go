package scheduler

import (
	"errors"
	"testing"
	"time"
)

// The execution log entries (#1849): AttemptStarted and AttemptFinished of
// the documented @types under cloud_scheduler_job, a failure at ERROR with
// its status and debugInfo, a success at INFO with neither.
func TestAttemptLogEntries(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	j := Job{Name: "projects/p1/locations/europe-west1/jobs/j1", PubSub: &PubSubTarget{Topic: "projects/p1/topics/t"}}
	s := AttemptStartedEntry(j, at, at)
	if s.GetLogName() != "projects/p1/logs/cloudscheduler.googleapis.com%2Fexecutions" {
		t.Errorf("logName = %q", s.GetLogName())
	}
	if r := s.GetResource(); r.GetType() != "cloud_scheduler_job" || r.GetLabels()["job_id"] != "j1" ||
		r.GetLabels()["location"] != "europe-west1" || r.GetLabels()["project_id"] != "p1" {
		t.Errorf("resource = %v", r)
	}
	f := s.GetJsonPayload().GetFields()
	if f["@type"].GetStringValue() != attemptStartedType || f["targetType"].GetStringValue() != "PUB_SUB" ||
		f["topicName"].GetStringValue() != "projects/p1/topics/t" || f["jobName"].GetStringValue() != j.Name {
		t.Errorf("AttemptStarted = %v", f)
	}

	ok := AttemptFinishedEntry(j, at, at, 0, nil)
	if f := ok.GetJsonPayload().GetFields(); ok.GetSeverity().String() != "INFO" || f["status"] != nil || f["debugInfo"] != nil ||
		f["@type"].GetStringValue() != attemptFinishedType {
		t.Errorf("a success's AttemptFinished = %v %v", ok.GetSeverity(), f)
	}

	h := Job{Name: "projects/p1/locations/us-central1/jobs/web", HTTP: &HTTPTarget{URI: "http://127.0.0.1:1/x"}}
	bad := AttemptFinishedEntry(h, at, at, 503, errors.New("returned HTTP 503"))
	f = bad.GetJsonPayload().GetFields()
	if bad.GetSeverity().String() != "ERROR" || f["status"].GetStringValue() != "UNAVAILABLE" ||
		f["debugInfo"].GetStringValue() != "URL_ERROR-ERROR_OTHER. Original HTTP response code number = 503" ||
		bad.GetHttpRequest().GetStatus() != 503 || f["url"].GetStringValue() != "http://127.0.0.1:1/x" {
		t.Errorf("a 503's AttemptFinished = %v %v %v", bad.GetSeverity(), f, bad.GetHttpRequest())
	}
	ae := AttemptFinishedEntry(Job{Name: "projects/p1/locations/us-central1/jobs/a", AppEngine: &AppEngineTarget{RelativeURI: "/t"}},
		at, at, 0, errNoAppEngine{url: "x"})
	if f := ae.GetJsonPayload().GetFields(); f["status"].GetStringValue() != "NOT_FOUND" || f["targetType"].GetStringValue() != "APP_ENGINE_HTTP" {
		t.Errorf("an App Engine failure's AttemptFinished = %v", f)
	}
}
