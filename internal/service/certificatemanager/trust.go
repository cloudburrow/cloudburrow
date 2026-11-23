package certificatemanager

import (
	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"google.golang.org/protobuf/proto"
)

// What Network Security's server TLS policies read from trust configs
// (#2160): a policy's mtlsPolicy.clientValidationTrustConfig names one,
// and the load balancers validate client certificates against its trust
// store and allowlist.

// TrustConfig is a trust config by name
// (projects/P/locations/L/trustConfigs/T), a copy; ok is false when there
// is none.
func (s *Server) TrustConfig(name string) (*cmpb.TrustConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.res[name].(*cmpb.TrustConfig)
	if !ok {
		return nil, false
	}
	return proto.Clone(t).(*cmpb.TrustConfig), true
}

// UseTrustUsers sets what names the resources that use a trust config (a
// server TLS policy), which keep it from being deleted.
func (s *Server) UseTrustUsers(f func(name string) []string) {
	s.mu.Lock()
	s.trustUsers = f
	s.mu.Unlock()
}

// UseCertificateUsers sets what names the resources beside the target
// proxies that use a certificate (a backend authentication config's
// clientCertificate), which keep it from being deleted.
func (s *Server) UseCertificateUsers(f func(name string) []string) {
	s.mu.Lock()
	s.certUsers = f
	s.mu.Unlock()
}
