package backupdr

// The Backup and DR trial (#1536): getTrial, trial.subscribe and trial.end,
// the Trial resource's documented state machine. A project's trial in a
// location (us, eu or asia, the locations the reference names) is ELIGIBLE
// until it is subscribed; subscribing makes it SUBSCRIBED for 30 days, after
// which it is EXPIRED; ending it before then makes it UNSUBSCRIBED with the
// endReason given. A trial is subscribed once: subscribing a trial that is
// not ELIGIBLE, or ending one that is not SUBSCRIBED, is FAILED_PRECONDITION.
// CloudBurrow bills nothing, so the trial changes no other method's answer.
//
// managementServers.msComplianceMetadata is here too: it answers whether
// the project is an Assured Workloads project, and CloudBurrow runs no
// Assured Workloads, so none is.

import (
	"net/http"
	"time"
)

// trialLength is how long a subscribed trial lasts: "Trial is expired post
// 30 days of subscription."
const trialLength = 30 * 24 * time.Hour

var trialLocations = map[string]bool{"us": true, "eu": true, "asia": true}

type trial struct {
	State, StartTime, EndTime, EndReason string
}

func (s *Server) checkTrialLocation(loc, location string) error {
	if !trialLocations[location] {
		return invalid("Location %s does not support a Backup and DR trial: the supported locations are us, eu and asia.", loc)
	}
	return nil
}

// trialJSON is the trial named loc+"/trial", with s.mu held.
func (s *Server) trialJSONLocked(loc string) map[string]any {
	out := map[string]any{"name": loc + "/trial", "state": "ELIGIBLE"}
	t := s.st.Trials[loc]
	if t == nil {
		return out
	}
	out["state"] = t.State
	out["startTime"] = t.StartTime
	out["endTime"] = t.EndTime
	if t.State == "SUBSCRIBED" {
		if end, err := time.Parse(time.RFC3339Nano, t.EndTime); err == nil && !s.now().Before(end) {
			out["state"] = "EXPIRED"
		}
	}
	if t.EndReason != "" {
		out["endReason"] = t.EndReason
	}
	return out
}

func (s *Server) getTrial(loc, location string) (any, error) {
	if err := s.checkTrialLocation(loc, location); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trialJSONLocked(loc), nil
}

func (s *Server) subscribeTrial(r *http.Request, loc, location string) (any, error) {
	if err := s.checkTrialLocation(loc, location); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.trialJSONLocked(loc); cur["state"] != "ELIGIBLE" {
		return nil, precondition("The trial of %s is %s: a project subscribes to a trial once.", loc, cur["state"])
	}
	now := s.now().UTC()
	if s.st.Trials == nil {
		s.st.Trials = map[string]*trial{}
	}
	s.st.Trials[loc] = &trial{State: "SUBSCRIBED", StartTime: now.Format(time.RFC3339Nano), EndTime: now.Add(trialLength).Format(time.RFC3339Nano)}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.trialJSONLocked(loc), nil
}

func (s *Server) endTrial(r *http.Request, loc, location string) (any, error) {
	if err := s.checkTrialLocation(loc, location); err != nil {
		return nil, err
	}
	var body struct {
		EndReason string `json:"endReason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.EndReason != "MOVE_TO_PAID" && body.EndReason != "DISCONTINUED" {
		return nil, invalid("endReason is required: MOVE_TO_PAID or DISCONTINUED, not %q.", body.EndReason)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.trialJSONLocked(loc); cur["state"] != "SUBSCRIBED" {
		return nil, precondition("The trial of %s is %s: only a SUBSCRIBED trial can be ended.", loc, cur["state"])
	}
	t := s.st.Trials[loc]
	t.State, t.EndReason, t.EndTime = "UNSUBSCRIBED", body.EndReason, s.now().UTC().Format(time.RFC3339Nano)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.trialJSONLocked(loc), nil
}

func (s *Server) msComplianceMetadata(r *http.Request) (any, error) {
	var body struct {
		ProjectID string `json:"projectId"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.ProjectID == "" {
		return nil, invalid("projectId is required.")
	}
	return map[string]any{"isAssuredWorkload": false}, nil
}
