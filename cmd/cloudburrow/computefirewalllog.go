package main

import (
	"context"
	"log"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// useFirewallLogging writes Compute Engine's Firewall Rules Logging
// records (#2180) to this instance's Cloud Logging through write (its own
// WriteLogEntries, so exclusions and sinks apply to them as to any write).
func (s *computeService) useFirewallLogging(write func(context.Context, *loggingpb.LogEntry) error) {
	s.firewallLog = func(e compute.FirewallLogEntry) {
		le, err := firewallLogEntry(e)
		if err != nil {
			log.Printf("compute: a firewall log entry: %v", err)
			return
		}
		if err := write(context.Background(), le); err != nil {
			log.Printf("compute: writing a firewall log entry: %v", err)
		}
	}
}

// firewallLogEntry is a Firewall Rules Logging record as Cloud Logging
// keeps it: compute.googleapis.com%2Ffirewall of a gce_subnetwork.
func firewallLogEntry(e compute.FirewallLogEntry) (*loggingpb.LogEntry, error) {
	payload, err := structpb.NewStruct(e.Payload)
	if err != nil {
		return nil, err
	}
	return &loggingpb.LogEntry{
		LogName:   compute.FirewallLogName(e.Project),
		Resource:  &monitoredres.MonitoredResource{Type: "gce_subnetwork", Labels: e.Labels},
		Timestamp: timestamppb.New(e.Time),
		Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
	}, nil
}
