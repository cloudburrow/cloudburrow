package main

// A Cloud Tasks queue's HTTP target in the console (#1282): the HTTP target
// group of the queue's Configuration tab, and the HTTP target section of
// Edit queue, which UpdateQueue saves with the mask path http_target. The
// URI override is written as one URL, whose scheme, host, port, path and
// query become uri_override's; an empty form clears the target.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// tasksHTTPTargetForm is a queue's http_target as the edit form holds it.
type tasksHTTPTargetForm struct{ uri, mode, method, headers, oidc string }

func tasksHTTPTargetValues(q tasks.Queue) tasksHTTPTargetForm {
	var f tasksHTTPTargetForm
	ht := tasks.QueueHTTPTarget(q)
	if ht == nil {
		return f
	}
	if o := ht.GetUriOverride(); o != nil {
		u := url.URL{Scheme: "https", Host: o.GetHost(), RawQuery: o.GetQueryOverride().GetQueryParams()}
		if o.Scheme != nil && o.GetScheme() == cloudtaskspb.UriOverride_HTTP {
			u.Scheme = "http"
		}
		if o.GetPort() > 0 {
			u.Host += ":" + strconv.FormatInt(o.GetPort(), 10)
		}
		u.Path = o.GetPathOverride().GetPath()
		f.uri = u.String()
		if o.GetUriOverrideEnforceMode() == cloudtaskspb.UriOverride_IF_NOT_EXISTS {
			f.mode = "IF_NOT_EXISTS"
		}
	}
	if m := ht.GetHttpMethod(); m != cloudtaskspb.HttpMethod_HTTP_METHOD_UNSPECIFIED {
		f.method = m.String()
	}
	if hs := ht.GetHeaderOverrides(); len(hs) > 0 {
		m := map[string]string{}
		for _, h := range hs {
			m[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
		b, _ := json.Marshal(m) // a map field's value is a JSON object
		f.headers = string(b)
	}
	f.oidc = ht.GetOidcToken().GetServiceAccountEmail()
	return f
}

// tasksHTTPTargetGroup is the HTTP target group of a queue's Configuration
// tab.
func tasksHTTPTargetGroup(q tasks.Queue) console.PropertyGroup {
	f := tasksHTTPTargetValues(q)
	or := func(v, none string) string {
		if v == "" {
			return none
		}
		return v
	}
	mode := ""
	if f.uri != "" {
		mode = or(f.mode, "ALWAYS")
	}
	return console.PropertyGroup{Heading: "HTTP target", Properties: []console.Property{
		{Label: "URI override", Value: or(f.uri, "None: each task's own URL")},
		{Label: "URI override mode", Value: or(mode, "—")},
		{Label: "HTTP method override", Value: or(f.method, "None")},
		{Label: "Header overrides", Value: or(f.headers, "None")},
		{Label: "OIDC token service account", Value: or(f.oidc, "None")},
	}}
}

// tasksHTTPTargetFromForm is the http_target Edit queue sends; nil, which
// clears it, when every field is empty.
func tasksHTTPTargetFromForm(values map[string]string) (*cloudtaskspb.HttpTarget, error) {
	uri := strings.TrimSpace(values["targetURI"])
	mode := strings.TrimSpace(values["targetMode"])
	method := strings.TrimSpace(values["targetMethod"])
	oidc := strings.TrimSpace(values["targetOidcAccount"])
	headers, err := console.ParseMap(values["targetHeaders"])
	if err != nil {
		return nil, fmt.Errorf("Header overrides: %w", err)
	}
	if uri == "" && method == "" && oidc == "" && len(headers) == 0 {
		return nil, nil
	}
	ht := &cloudtaskspb.HttpTarget{}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, errors.New("URI override must be an http or https URL with a host")
		}
		o := &cloudtaskspb.UriOverride{Host: proto.String(u.Hostname()),
			PathOverride:  &cloudtaskspb.PathOverride{Path: u.EscapedPath()},
			QueryOverride: &cloudtaskspb.QueryOverride{QueryParams: u.RawQuery}}
		o.Scheme = cloudtaskspb.UriOverride_HTTPS.Enum()
		if u.Scheme == "http" {
			o.Scheme = cloudtaskspb.UriOverride_HTTP.Enum()
		}
		if p := u.Port(); p != "" {
			n, _ := strconv.ParseInt(p, 10, 64)
			o.Port = proto.Int64(n)
		}
		switch mode {
		case "", "ALWAYS":
			o.UriOverrideEnforceMode = cloudtaskspb.UriOverride_ALWAYS
		case "IF_NOT_EXISTS":
			o.UriOverrideEnforceMode = cloudtaskspb.UriOverride_IF_NOT_EXISTS
		default:
			return nil, fmt.Errorf("URI override mode %q is not ALWAYS or IF_NOT_EXISTS", mode)
		}
		ht.UriOverride = o
	}
	if method != "" {
		m, ok := cloudtaskspb.HttpMethod_value[method]
		if !ok || m == 0 {
			return nil, fmt.Errorf("HTTP method override %q is not one of %s", method, tasksHTTPMethods)
		}
		ht.HttpMethod = cloudtaskspb.HttpMethod(m)
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ht.HeaderOverrides = append(ht.HeaderOverrides, &cloudtaskspb.HttpTarget_HeaderOverride{
			Header: &cloudtaskspb.HttpTarget_Header{Key: k, Value: headers[k]}})
	}
	if oidc != "" {
		ht.AuthorizationHeader = &cloudtaskspb.HttpTarget_OidcToken{OidcToken: &cloudtaskspb.OidcToken{ServiceAccountEmail: oidc}}
	}
	return ht, nil
}
