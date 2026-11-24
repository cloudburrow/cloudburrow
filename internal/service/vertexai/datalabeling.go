package vertexai

// Agent Platform data labeling jobs (#2214): JobService's DataLabelingJob
// methods. Google has shut the service down: "Vertex AI Data Labeling
// Service", deprecated June 30, 2023, shut down October 3, 2024; "For new
// labeling tasks, you can add labels using the Google Cloud console"
// (https://docs.cloud.google.com/vertex-ai/docs/deprecations, read
// 2026-11-24). No job can exist, so a create is refused, a list is empty,
// and a get, delete or cancel is NOT_FOUND.
//
// UNVERIFIED: Google documents no error for the shut-down methods; the
// FAILED_PRECONDITION and its wording are CloudBurrow's (docs/gaps.md).

import (
	"context"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func dataLabelingShutDown() error {
	return apierror.FailedPrecondition("Vertex AI Data Labeling Service was shut down on October 3, 2024 " +
		"(https://cloud.google.com/vertex-ai/docs/deprecations): add labels to a dataset in the Google Cloud console instead")
}

func (x *jobService) CreateDataLabelingJob(_ context.Context, req *aiplatformpb.CreateDataLabelingJobRequest) (*aiplatformpb.DataLabelingJob, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	return nil, grpcError(dataLabelingShutDown())
}

func dataLabelingJobNotFound(name string) error {
	if _, _, _, err := parseChild("name", "dataLabelingJobs", name); err != nil {
		return err
	}
	return apierror.NotFound("DataLabelingJob %s not found", name)
}

func (x *jobService) GetDataLabelingJob(_ context.Context, req *aiplatformpb.GetDataLabelingJobRequest) (*aiplatformpb.DataLabelingJob, error) {
	return nil, grpcError(dataLabelingJobNotFound(req.GetName()))
}

func (x *jobService) ListDataLabelingJobs(_ context.Context, req *aiplatformpb.ListDataLabelingJobsRequest) (*aiplatformpb.ListDataLabelingJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListDataLabelingJobsResponse{}, nil
}

func (x *jobService) DeleteDataLabelingJob(_ context.Context, req *aiplatformpb.DeleteDataLabelingJobRequest) (*longrunningpb.Operation, error) {
	return nil, grpcError(dataLabelingJobNotFound(req.GetName()))
}

func (x *jobService) CancelDataLabelingJob(_ context.Context, req *aiplatformpb.CancelDataLabelingJobRequest) (*emptypb.Empty, error) {
	return nil, grpcError(dataLabelingJobNotFound(req.GetName()))
}
