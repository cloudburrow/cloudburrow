package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

// TestConsoleTasksLoggingSampling (#1256): Edit queue sets the queue's
// stackdriver_logging_config sampling ratio, the page shows it, and an
// empty field clears it.
func TestConsoleTasksLoggingSampling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	d, _ := p.Detail(ctx, "edit-proj", []string{name})
	values := submittedValues(d.Edit)
	if values["samplingRatio"] != "" {
		t.Errorf("a new queue's sampling ratio is prefilled as %q", values["samplingRatio"])
	}
	values["samplingRatio"] = "0.5"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatal(err)
	}
	q, err := p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name})
	if err != nil || q.GetStackdriverLoggingConfig().GetSamplingRatio() != 0.5 {
		t.Fatalf("GetQueue logging = %v, %v", q.GetStackdriverLoggingConfig(), err)
	}
	d, _ = p.Detail(ctx, "edit-proj", []string{name})
	if !strings.Contains(detailText(d), "0.5 of attempts written to Cloud Logging") {
		t.Error("the page does not show the sampling ratio")
	}
	if got := submittedValues(d.Edit)["samplingRatio"]; got != "0.5" {
		t.Errorf("the form is prefilled with %q", got)
	}
	values["samplingRatio"] = "2"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err == nil || !strings.Contains(err.Error(), "sampling_ratio") {
		t.Errorf("a ratio of 2 refused with %v", err)
	}
	values["samplingRatio"] = ""
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatal(err)
	}
	if q, _ = p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name}); q.GetStackdriverLoggingConfig() != nil {
		t.Errorf("an empty ratio left %v", q.GetStackdriverLoggingConfig())
	}
}
