package tasks

// A queue-level http_target (#1282), to the HttpTarget and UriOverride
// reference. The queue keeps it as the message's proto JSON, and the
// dispatcher applies it to each attempt of each of the queue's tasks, from
// the queue as it is at that attempt; the task keeps its own httpRequest,
// which GetTask returns unchanged (UNVERIFIED: no page says whether Google
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/tasks/v2/target.proto
// rewrites the stored task).
//
//   - uri_override replaces the scheme, host, port (0 clears it), path and
//     query it names; in IF_NOT_EXISTS mode only for a task with no url,
//     and in ALWAYS (the default) for every task. A task may be created
//     with no url on a queue whose uri_override names a host.
//   - http_method, when set, replaces the task's.
//   - header_overrides set those headers on every attempt, over the task's
//     own.
//   - oauth_token or oidc_token replaces the task's own token, and any
//     Authorization header it sets.

import (
	"net/url"
	"strconv"
	"strings"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// checkHTTPTarget refuses a queue http_target no dispatch could follow.
func checkHTTPTarget(p *taskspb.Queue) error {
	ht := p.GetHttpTarget()
	if ht == nil {
		return nil
	}
	if o := ht.GetUriOverride(); o != nil {
		if o.Host != nil && o.GetHost() == "" {
			return apierror.InvalidArgument("http_target.uri_override.host cannot be an empty string")
		}
		if o.GetPort() < 0 || o.GetPort() > 65535 {
			return apierror.InvalidArgument("http_target.uri_override.port %d must be between 0 and 65535", o.GetPort())
		}
	}
	for i, h := range ht.GetHeaderOverrides() {
		if strings.TrimSpace(h.GetHeader().GetKey()) == "" {
			return apierror.InvalidArgument("http_target.header_overrides[%d].header.key is required", i)
		}
	}
	switch a := ht.GetAuthorizationHeader().(type) {
	case *taskspb.HttpTarget_OidcToken:
		if a.OidcToken.GetServiceAccountEmail() == "" {
			return apierror.InvalidArgument("http_target.oidc_token.service_account_email is required")
		}
	case *taskspb.HttpTarget_OauthToken:
		if a.OauthToken.GetServiceAccountEmail() == "" {
			return apierror.InvalidArgument("http_target.oauth_token.service_account_email is required")
		}
	}
	return nil
}

// httpTargetJSON is how a Queue keeps an http_target; nil for none.
func httpTargetJSON(ht *taskspb.HttpTarget) []byte {
	if ht == nil || proto.Size(ht) == 0 {
		return nil
	}
	b, _ := protojson.Marshal(ht)
	return b
}

// queueHTTPTarget reads a Queue's http_target; nil for none.
func queueHTTPTarget(q Queue) *taskspb.HttpTarget {
	if len(q.HTTPTarget) == 0 {
		return nil
	}
	ht := &taskspb.HttpTarget{}
	if err := protojson.Unmarshal(q.HTTPTarget, ht); err != nil {
		return nil
	}
	return ht
}

// targetsHost says whether a queue's http_target can make a task's whole
// URL: its uri_override names a host.
func targetsHost(q Queue) bool {
	ht := queueHTTPTarget(q)
	return ht.GetUriOverride().GetHost() != ""
}

// withQueueTarget is the httpRequest one attempt of task sends under its
// queue's http_target: a copy, the task's own left as it is.
func withQueueTarget(hr HTTPRequest, q Queue) HTTPRequest {
	ht := queueHTTPTarget(q)
	if ht == nil {
		return hr
	}
	if o := ht.GetUriOverride(); o != nil &&
		(o.GetUriOverrideEnforceMode() != taskspb.UriOverride_IF_NOT_EXISTS || hr.URL == "") {
		hr.URL = overrideURI(hr.URL, o)
	}
	if m := ht.GetHttpMethod(); m != taskspb.HttpMethod_HTTP_METHOD_UNSPECIFIED {
		hr.Method = m.String()
		if m == taskspb.HttpMethod_GET || m == taskspb.HttpMethod_HEAD || m == taskspb.HttpMethod_DELETE || m == taskspb.HttpMethod_OPTIONS {
			hr.Body = nil // "the body of the task will be ignored"
		}
	}
	if hs := ht.GetHeaderOverrides(); len(hs) > 0 {
		headers := make(map[string]string, len(hr.Headers)+len(hs))
		for k, v := range hr.Headers {
			headers[k] = v
		}
		for _, h := range hs {
			for k := range headers {
				if strings.EqualFold(k, h.GetHeader().GetKey()) {
					delete(headers, k)
				}
			}
			headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
		hr.Headers = headers
	}
	switch a := ht.GetAuthorizationHeader().(type) {
	case *taskspb.HttpTarget_OidcToken:
		hr.OIDCToken, hr.OAuthToken = &OIDCToken{ServiceAccountEmail: a.OidcToken.GetServiceAccountEmail(), Audience: a.OidcToken.GetAudience()}, nil
	case *taskspb.HttpTarget_OauthToken:
		hr.OIDCToken, hr.OAuthToken = nil, &OAuthToken{ServiceAccountEmail: a.OauthToken.GetServiceAccountEmail(), Scope: a.OauthToken.GetScope()}
	}
	return hr
}

// overrideURI applies a uri_override to a task's URL, or to none.
func overrideURI(raw string, o *taskspb.UriOverride) string {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		u = &url.URL{Scheme: "https"}
	}
	if o.Scheme != nil && o.GetScheme() != taskspb.UriOverride_SCHEME_UNSPECIFIED {
		u.Scheme = strings.ToLower(o.GetScheme().String())
	}
	host, port := u.Hostname(), u.Port()
	if o.Host != nil {
		host = o.GetHost()
	}
	if o.Port != nil {
		port = ""
		if o.GetPort() > 0 {
			port = strconv.FormatInt(o.GetPort(), 10)
		}
	}
	u.Host = host
	if port != "" {
		u.Host = host + ":" + port
	}
	if p := o.GetPathOverride(); p != nil {
		u.Path, u.RawPath = p.GetPath(), ""
	}
	if q := o.GetQueryOverride(); q != nil {
		u.RawQuery = q.GetQueryParams()
	}
	return u.String()
}

// QueueHTTPTarget is a queue's http_target, for callers outside the API
// such as the console; nil for none.
func QueueHTTPTarget(q Queue) *taskspb.HttpTarget { return queueHTTPTarget(q) }
