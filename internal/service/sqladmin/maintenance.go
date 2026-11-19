package sqladmin

// instances.rescheduleMaintenance (#1524). Cloud SQL moves a maintenance
// event it has scheduled: IMMEDIATE (within five minutes),
// NEXT_AVAILABLE_WINDOW (a week on) or SPECIFIC_TIME (scheduleTime,
// within the rescheduling duration). CloudBurrow schedules no maintenance
// event: an instance's server is its version's pinned image, replaced when
// CloudBurrow itself is upgraded, so scheduledMaintenance is never set and
// there is no event to move. A request is checked as Google documents it
// (reschedule and its rescheduleType are required; scheduleTime is an RFC
// 3339 timestamp, used with SPECIFIC_TIME), on an instance that exists,
// and then refused with 501.

import (
	"net/http"
	"time"
)

type rescheduleBody struct {
	Reschedule *struct {
		RescheduleType string `json:"rescheduleType"`
		ScheduleTime   string `json:"scheduleTime"`
	} `json:"reschedule"`
}

// rescheduleMaintenance answers for an instance that exists (the router
// has found it).
func rescheduleMaintenance(r *http.Request) (any, error) {
	var b rescheduleBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Reschedule == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule is required.")
	}
	switch b.Reschedule.RescheduleType {
	case "IMMEDIATE", "NEXT_AVAILABLE_WINDOW":
	case "SPECIFIC_TIME":
		if b.Reschedule.ScheduleTime == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.scheduleTime is required with rescheduleType SPECIFIC_TIME.")
		}
		if _, err := time.Parse(time.RFC3339Nano, b.Reschedule.ScheduleTime); err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid value at 'reschedule.schedule_time' (type.googleapis.com/google.protobuf.Timestamp), Field 'scheduleTime', Invalid time format: %q", b.Reschedule.ScheduleTime)
		}
	case "", "RESCHEDULE_TYPE_UNSPECIFIED":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.rescheduleType is required: IMMEDIATE, NEXT_AVAILABLE_WINDOW or SPECIFIC_TIME.")
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid value at 'reschedule.reschedule_type' (type.googleapis.com/google.cloud.sql.v1.SqlInstancesRescheduleMaintenanceRequestBody.RescheduleType), %q", b.Reschedule.RescheduleType)
	}
	return nil, unimplemented("instances.rescheduleMaintenance", verbWhy("rescheduleMaintenance"))
}
