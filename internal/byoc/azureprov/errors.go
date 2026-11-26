package azureprov

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// actionPattern finds the action an AuthorizationFailed message names:
// "does not have authorization to perform action 'X' over scope ...".
var actionPattern = regexp.MustCompile(`perform action '([^']+)'`)

// quotaCodes are Azure's refusals for want of quota or capacity.
var quotaCodes = map[string]bool{
	"QuotaExceeded": true, "OperationNotAllowed": true, "SkuNotAvailable": true,
	"AllocationFailed": true, "ZonalAllocationFailed": true, "OverconstrainedAllocationRequest": true,
	"OverconstrainedZonalAllocationRequest": true, "PublicIPCountLimitReached": true,
}

// mapError turns Azure's answer into byoc's errors: a missing permission, a
// quota, or a sign-in that is no longer accepted. what says what was being
// done, in plain words.
func mapError(what string, err error) error {
	if err == nil {
		return nil
	}
	var auth *byoc.AuthError
	if errors.As(err, &auth) {
		return err
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		return fmt.Errorf("%s: %w", what, err)
	}
	msg := responseMessage(re)
	switch {
	case re.StatusCode == http.StatusUnauthorized || re.ErrorCode == "InvalidAuthenticationToken" ||
		re.ErrorCode == "ExpiredAuthenticationToken":
		return &byoc.AuthError{Message: what + ": " + msg}
	case re.ErrorCode == "AuthorizationFailed" || re.ErrorCode == "LinkedAuthorizationFailed" ||
		(re.StatusCode == http.StatusForbidden && re.ErrorCode == ""):
		pe := &byoc.PermissionError{Message: what + ": " + msg}
		if m := actionPattern.FindStringSubmatch(msg); m != nil {
			pe.Missing = []byoc.Permission{{Action: m[1], Why: what}}
		}
		return pe
	case quotaCodes[re.ErrorCode] && (re.ErrorCode != "OperationNotAllowed" || mentionsQuota(msg)):
		return &byoc.QuotaError{Message: what + ": " + re.ErrorCode + ": " + msg}
	}
	return fmt.Errorf("%s: %s: %s", what, re.ErrorCode, msg)
}

func mentionsQuota(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "quota") || strings.Contains(m, "limit") || strings.Contains(m, "allocat")
}

// responseMessage is the error body's message (azcore has read the body
// into Error()), or the code, or the status.
func responseMessage(re *azcore.ResponseError) string {
	if m := messagePattern.FindStringSubmatch(re.Error()); m != nil {
		return m[1]
	}
	if re.ErrorCode != "" {
		return re.ErrorCode
	}
	return http.StatusText(re.StatusCode)
}

var messagePattern = regexp.MustCompile(`"message":\s*"((?:[^"\\]|\\.)*)"`)

// wait polls p until it is done, at the driver's pace and clock, rather than
// PollUntilDone's, so tests run without waiting.
func wait[T any](ctx context.Context, o Options, p *runtime.Poller[T]) (T, error) {
	for !p.Done() {
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-o.Clock.After(o.PollInterval):
		}
		if _, err := p.Poll(ctx); err != nil {
			var zero T
			return zero, err
		}
	}
	return p.Result(ctx)
}

// isNotFound reports a 404.
func isNotFound(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

// errorCode is the ARM error code, or "".
func errorCode(err error) string {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return re.ErrorCode
	}
	return ""
}
