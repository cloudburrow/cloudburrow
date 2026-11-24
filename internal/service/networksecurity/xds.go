package networksecurity

import (
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/proto"
)

// ClientTLSPolicy is a client TLS policy by name
// (projects/P/locations/L/clientTlsPolicies/X), a copy: what the xDS API
// gives a proxyless gRPC client of a backend service whose
// securitySettings name it (#2160).
func (s *Server) ClientTLSPolicy(name string) (*nspb.ClientTlsPolicy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.res[name].(*nspb.ClientTlsPolicy)
	if !ok {
		return nil, false
	}
	return proto.Clone(p).(*nspb.ClientTlsPolicy), true
}
