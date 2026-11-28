package datamigration

import (
	"context"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Conversion workspaces and their mapping rules are not served: a
// conversion workspace converts an Oracle (or SQL Server) schema to
// PostgreSQL with Google's conversion engine, for a heterogeneous
// migration. CloudBurrow has neither that engine nor an Oracle server to
// seed a workspace from, so every method answers UNIMPLEMENTED, listing
// included: an empty list would say the API holds none where it cannot
// hold any (docs/gaps.md).
const conversionWhy = "conversion workspaces convert an Oracle or SQL Server schema to PostgreSQL with Google's conversion engine, " +
	"which CloudBurrow does not have; homogeneous migrations (MySQL and PostgreSQL) need none (docs/gaps.md)"

func conversion(method string) error {
	return unimplemented("conversionWorkspaces."+method, conversionWhy)
}

func (s *Server) ListConversionWorkspaces(context.Context, *dmspb.ListConversionWorkspacesRequest) (*dmspb.ListConversionWorkspacesResponse, error) {
	return nil, conversion("list")
}

func (s *Server) GetConversionWorkspace(context.Context, *dmspb.GetConversionWorkspaceRequest) (*dmspb.ConversionWorkspace, error) {
	return nil, conversion("get")
}

func (s *Server) CreateConversionWorkspace(context.Context, *dmspb.CreateConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("create")
}

func (s *Server) UpdateConversionWorkspace(context.Context, *dmspb.UpdateConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("patch")
}

func (s *Server) DeleteConversionWorkspace(context.Context, *dmspb.DeleteConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("delete")
}

func (s *Server) CreateMappingRule(context.Context, *dmspb.CreateMappingRuleRequest) (*dmspb.MappingRule, error) {
	return nil, conversion("mappingRules.create")
}

func (s *Server) DeleteMappingRule(context.Context, *dmspb.DeleteMappingRuleRequest) (*emptypb.Empty, error) {
	return nil, conversion("mappingRules.delete")
}

func (s *Server) ListMappingRules(context.Context, *dmspb.ListMappingRulesRequest) (*dmspb.ListMappingRulesResponse, error) {
	return nil, conversion("mappingRules.list")
}

func (s *Server) GetMappingRule(context.Context, *dmspb.GetMappingRuleRequest) (*dmspb.MappingRule, error) {
	return nil, conversion("mappingRules.get")
}

func (s *Server) SeedConversionWorkspace(context.Context, *dmspb.SeedConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("seed")
}

func (s *Server) ImportMappingRules(context.Context, *dmspb.ImportMappingRulesRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("mappingRules.import")
}

func (s *Server) ConvertConversionWorkspace(context.Context, *dmspb.ConvertConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("convert")
}

func (s *Server) CommitConversionWorkspace(context.Context, *dmspb.CommitConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("commit")
}

func (s *Server) RollbackConversionWorkspace(context.Context, *dmspb.RollbackConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("rollback")
}

func (s *Server) ApplyConversionWorkspace(context.Context, *dmspb.ApplyConversionWorkspaceRequest) (*longrunningpb.Operation, error) {
	return nil, conversion("apply")
}

func (s *Server) DescribeDatabaseEntities(context.Context, *dmspb.DescribeDatabaseEntitiesRequest) (*dmspb.DescribeDatabaseEntitiesResponse, error) {
	return nil, conversion("describeDatabaseEntities")
}

func (s *Server) SearchBackgroundJobs(context.Context, *dmspb.SearchBackgroundJobsRequest) (*dmspb.SearchBackgroundJobsResponse, error) {
	return nil, conversion("searchBackgroundJobs")
}

func (s *Server) DescribeConversionWorkspaceRevisions(context.Context, *dmspb.DescribeConversionWorkspaceRevisionsRequest) (*dmspb.DescribeConversionWorkspaceRevisionsResponse, error) {
	return nil, conversion("describeConversionWorkspaceRevisions")
}

func (s *Server) FetchStaticIps(_ context.Context, req *dmspb.FetchStaticIpsRequest) (*dmspb.FetchStaticIpsResponse, error) {
	if _, _, err := checkParent("name", req.GetName()); err != nil {
		return nil, err
	}
	if s.opts.StaticIPs == nil {
		return nil, unimplemented("locations.fetchStaticIps", "this instance does not know the address its destinations connect from")
	}
	out, next, err := pageStrings("dms-ips:"+req.GetName(), s.opts.StaticIPs(), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &dmspb.FetchStaticIpsResponse{StaticIps: out, NextPageToken: next}, nil
}
