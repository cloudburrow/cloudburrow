package iamcheck

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InsufficientScopes is Google's refusal of a token whose scopes do not
// cover the call.
func InsufficientScopes(service, method string) error {
	st := status.New(codes.PermissionDenied, "Request had insufficient authentication scopes.")
	md := map[string]string{"service": service}
	if method != "" {
		md["method"] = method
	}
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: "ACCESS_TOKEN_SCOPE_INSUFFICIENT", Domain: "googleapis.com", Metadata: md}); err == nil {
		st = d
	}
	return st.Err()
}
