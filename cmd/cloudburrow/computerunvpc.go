package main

import (
	"errors"

	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// computeRunVPC is Cloud Run's view of the VPC (#2140): Serverless VPC
// Access's connectors and Compute Engine's networks, read when a service
// or job is deployed, so it works whichever of the two starts first.
type computeRunVPC struct{ svc *computeService }

func (v computeRunVPC) ConnectorReady(name string) error {
	va := v.svc.VPCAccess()
	if va == nil {
		return errors.New(computeNotStarted)
	}
	c, ok := va.Connector(name)
	if !ok {
		return apierror.InvalidArgument("vpc_access.connector: connector %s was not found", name)
	}
	if c.GetState() != vpcaccesspb.Connector_READY {
		return apierror.FailedPrecondition("vpc_access.connector: connector %s is %s, not READY", name, c.GetState())
	}
	return nil
}

func (v computeRunVPC) HasNetwork(ref, project string) bool {
	api := v.svc.API()
	return api != nil && api.HasNetwork(ref, project)
}

func (v computeRunVPC) Subnetwork(ref, project, region string) (string, bool) {
	api := v.svc.API()
	if api == nil {
		return "", false
	}
	return api.Subnetwork(ref, project, region)
}
