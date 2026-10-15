package logging

import (
	"context"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/label"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// monitoredResourceDescriptors are the descriptors
// ListMonitoredResourceDescriptors returns (#1215): Google's own, as its
// monitored resource list documents them
// (https://cloud.google.com/logging/docs/api/v2/resource-list), for the
// resource types CloudBurrow itself writes entries for. CloudBurrow's
// services write no log entries of their own, so that is "global", the
// resource the official clients default to. An application may write an
// entry with any resource type; WriteLogEntries does not check it against
// this list, which says what CloudBurrow knows, not what it accepts.
var monitoredResourceDescriptors = []*mrpb.MonitoredResourceDescriptor{
	{
		Type:        "global",
		DisplayName: "Global",
		Description: "A global resource type, for log entries not associated with a specific resource.",
		Labels: []*label.LabelDescriptor{{
			Key:         "project_id",
			ValueType:   label.LabelDescriptor_STRING,
			Description: `The identifier of the GCP project associated with this resource, such as "my-project".`,
		}},
	},
}

// ListMonitoredResourceDescriptors lists the descriptors above, paged by
// page_size and page_token like the other lists.
func (s *Server) ListMonitoredResourceDescriptors(_ context.Context, req *loggingpb.ListMonitoredResourceDescriptorsRequest) (*loggingpb.ListMonitoredResourceDescriptorsResponse, error) {
	byType := map[string]*mrpb.MonitoredResourceDescriptor{}
	types := make([]string, 0, len(monitoredResourceDescriptors))
	for _, d := range monitoredResourceDescriptors {
		byType[d.GetType()] = d
		types = append(types, d.GetType())
	}
	page, next, err := paging.Page("logging:monitoredResourceDescriptors", types, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &loggingpb.ListMonitoredResourceDescriptorsResponse{NextPageToken: next}
	for _, t := range page {
		resp.ResourceDescriptors = append(resp.ResourceDescriptors, byType[t])
	}
	return resp, nil
}
