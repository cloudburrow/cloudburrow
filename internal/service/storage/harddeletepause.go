package storage

import (
	"net/http"
	"time"
)

// The rules of softDeletePolicy.hardDeletePause (#1715), from
// https://docs.cloud.google.com/storage/docs/pause-hard-deletions (re-read
// 2026-11-17):
//
//   - "Hard deletions can't be paused during bucket creation or for buckets
//     that don't have soft delete enabled."
//   - "You can pause and resume hard deletions on a bucket a maximum of once
//     every 24 hours": read here as one change of the pause, either way, per
//     24 hours of the server's clock, counted from the last change.
//   - "A bucket can't be deleted while hard deletions are paused."
//   - "You can update the soft delete retention duration while hard
//     deletions are paused. However, you can't disable soft delete while
//     hard deletions are paused."
//   - "If any objects have passed their retention duration, they're
//     permanently deleted when you resume hard deletions": pastHardDelete
//     and Sweep (softdelete.go).
//
// The page lists no status or reason for any refusal, so each is
// UNVERIFIED (docs re-read 2026-11-17), coded as CloudBurrow codes its
// nearest refusal: a field a create may not set is 400 (as a change of a
// create-only field is), a bucket without soft delete is the status-codes
// page's 412 softDeletePolicyNotSet, a change the bucket's state does not
// allow is 412 conditionNotMet, and buckets.delete is 409, as for a bucket
// that is not empty.

// hardDeletePauseInterval is how often the pause may change.
const hardDeletePauseInterval = 24 * time.Hour

// pauseRequested is whether f's soft delete policy asks for the pause.
func pauseRequested(f map[string]any) bool {
	return hardDeletePaused(bucketRecord{Fields: f})
}

// checkHardDeletePauseCreate refuses the pause in buckets.insert.
func checkHardDeletePauseCreate(f map[string]any) error {
	if pauseRequested(f) {
		return badRequest("Hard deletions can't be paused during bucket creation.")
	}
	return nil
}

// pauseState is what a bucket update is checked against: the bucket's
// pause and soft delete retention before the update, and when the pause
// last changed.
type pauseState struct {
	paused    bool
	retention time.Duration
	changed   time.Time
}

func pauseStateOf(b bucketRecord) pauseState {
	return pauseState{paused: hardDeletePaused(b), retention: softRetention(b), changed: b.HardDeletePauseChanged}
}

// check checks an update of bucket name to the fields next at now, and
// reports whether it changes the pause.
func (old pauseState) check(name string, next map[string]any, now time.Time) (bool, error) {
	was, will := old.paused, pauseRequested(next)
	retention := softRetention(bucketRecord{Fields: next})
	if was && retention == 0 {
		return false, errorf(http.StatusPreconditionFailed, "conditionNotMet",
			"Soft delete can't be disabled while hard deletions are paused for bucket %s.", name)
	}
	if was == will {
		return false, nil
	}
	if will && (old.retention == 0 || retention == 0) {
		return false, errorf(http.StatusPreconditionFailed, "softDeletePolicyNotSet",
			"Hard deletions can't be paused for bucket %s, which doesn't have soft delete enabled.", name)
	}
	if last := old.changed; !last.IsZero() && now.Before(last.Add(hardDeletePauseInterval)) {
		verb := "resumed"
		if will {
			verb = "paused"
		}
		return false, errorf(http.StatusPreconditionFailed, "conditionNotMet",
			"Hard deletions for bucket %s can't be %s until %s: they can be paused and resumed at most once every 24 hours.",
			name, verb, rfc3339(last.Add(hardDeletePauseInterval)))
	}
	return true, nil
}

// checkBucketDeletePause refuses buckets.delete while hard deletions are
// paused.
func checkBucketDeletePause(b bucketRecord) error {
	if hardDeletePaused(b) {
		return conflict("The bucket %s can't be deleted while hard deletions are paused.", b.Name)
	}
	return nil
}
