package awsprov

import (
	"errors"
	"strings"

	"github.com/aws/smithy-go"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// mapErr turns an AWS error into byoc's: a missing permission, a quota or
// capacity refusal, or credentials AWS no longer accepts. action is the IAM
// action the call needs, named in a permission error.
func mapErr(action string, err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		return err
	}
	code, msg := api.ErrorCode(), api.ErrorMessage()
	switch code {
	case "UnauthorizedOperation", "AccessDenied", "AccessDeniedException", "AuthorizationError", "ForbiddenException":
		return &byoc.PermissionError{
			Missing: []byoc.Permission{{Action: action, Why: whyOf(action)}},
			Message: msg,
		}
	case "VcpuLimitExceeded", "InstanceLimitExceeded", "InsufficientInstanceCapacity", "MaxSpotInstanceCountExceeded",
		"VpcLimitExceeded", "AddressLimitExceeded", "InternetGatewayLimitExceeded", "RulesPerSecurityGroupLimitExceeded",
		"SecurityGroupLimitExceeded", "VolumeLimitExceeded", "Unsupported":
		return &byoc.QuotaError{Message: code + ": " + msg}
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException",
		"AuthFailure", "UnauthorizedException", "InvalidGrantException", "SignatureDoesNotMatch", "RequestExpired":
		return &byoc.AuthError{Message: code + ": " + msg}
	}
	if strings.HasSuffix(code, "LimitExceeded") {
		return &byoc.QuotaError{Message: code + ": " + msg}
	}
	return err
}

// apiCode is err's AWS error code, or "".
func apiCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}
