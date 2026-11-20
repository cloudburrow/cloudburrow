package sysparams

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Params are a request's system parameters
// (https://docs.cloud.google.com/apis/docs/system-parameters).
type Params struct {
	// Fields is the partial-response selection: fields or $fields, else
	// the X-Goog-FieldMask header. Nil selects everything.
	Fields Mask
	// Alt is alt or $alt: "" (json), json, media, proto or sse, with any
	// ";enum-encoding=int" suffix removed into EnumInt.
	Alt     string
	EnumInt bool
	// Pretty is prettyPrint / $prettyPrint, which Google defaults to true.
	// PrettySet says whether the request named it.
	Pretty, PrettySet bool
	// Callback is the JSONP callback (callback / $callback), "" for none.
	Callback string
	// XGAFV is the error format, 1 or 2 ($.xgafv; 2 by default).
	XGAFV int
	// OutputDefaults is $outputDefaults: write fields at their defaults.
	OutputDefaults bool
}

// Accepted are the system parameters every API takes and that change
// nothing here: quota and billing attribution (quotaUser, userIp,
// $userProject), API keys (key, $key), credentials sent in the query
// (access_token, oauth_token: CloudBurrow reads the Authorization header),
// $unique (no caching to disable) and the upload protocol markers the
// discovery clients send.
var Accepted = map[string]bool{
	"quotaUser": true, "$quotaUser": true, "userIp": true, "$userIp": true, "$userProject": true,
	"key": true, "$key": true, "access_token": true, "oauth_token": true, "$unique": true,
	"upload_protocol": true, "uploadType": true,
}

// Handled are the parameters Params reads.
var Handled = map[string]bool{
	"fields": true, "$fields": true, "alt": true, "$alt": true, "prettyPrint": true, "$prettyPrint": true,
	"callback": true, "$callback": true, "$.xgafv": true, "$outputDefaults": true,
}

// IsSystem reports whether name is a system parameter Params reads or one
// Accepted ignores.
func IsSystem(name string) bool { return Handled[name] || Accepted[name] }

var callbackRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*|\[[0-9]+\])*$`)

// Query splits a raw query into its parameters, keeping a ';' inside a
// value (Go's url.ParseQuery refuses it, and the GAPIC REST clients send
// $alt=json;enum-encoding=int).
func Query(raw string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			return nil, apierror.InvalidArgument("the query is not validly escaped")
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			return nil, apierror.InvalidArgument("the query is not validly escaped")
		}
		out[name] = append(out[name], val)
	}
	return out, nil
}

// FromRequest reads r's system parameters. A malformed one is
// INVALID_ARGUMENT, naming it: a repeated one, a fields value that does not
// parse, a prettyPrint, $outputDefaults or $.xgafv value out of range, or a
// callback that is not a JavaScript name.
func FromRequest(r *http.Request) (Params, error) {
	vals, err := Query(r.URL.RawQuery)
	if err != nil {
		return Params{XGAFV: 2, Pretty: true}, err
	}
	return FromValues(vals, r.Header)
}

// FromValues is FromRequest of parsed query values and headers.
func FromValues(vals map[string][]string, h http.Header) (Params, error) {
	p := Params{XGAFV: 2, Pretty: true}
	one := func(names ...string) (string, string, bool, error) {
		var name, v string
		var found bool
		for _, n := range names {
			vs, ok := vals[n]
			if !ok {
				continue
			}
			if len(vs) > 1 || found {
				return n, "", false, apierror.InvalidArgument("query parameter %q is repeated", n)
			}
			name, v, found = n, vs[0], true
		}
		return name, v, found, nil
	}
	// $.xgafv first, so a later error is written in the format asked for.
	if n, v, ok, err := one("$.xgafv"); err != nil {
		return p, err
	} else if ok {
		switch v {
		case "1":
			p.XGAFV = 1
		case "2":
		default:
			return p, apierror.InvalidArgument("Invalid value at '%s' (type.googleapis.com/Xgafv), %q", n, v)
		}
	}
	if _, v, ok, err := one("callback", "$callback"); err != nil {
		return p, err
	} else if ok {
		if !callbackRE.MatchString(v) {
			return p, apierror.InvalidArgument("Invalid JSONP callback name %q", v)
		}
		p.Callback = v
	}
	if n, v, ok, err := one("prettyPrint", "$prettyPrint"); err != nil {
		return p, err
	} else if ok {
		b, perr := boolOf(n, v)
		if perr != nil {
			return p, perr
		}
		p.Pretty, p.PrettySet = b, true
	}
	if n, v, ok, err := one("$outputDefaults"); err != nil {
		return p, err
	} else if ok {
		b, perr := boolOf(n, v)
		if perr != nil {
			return p, perr
		}
		p.OutputDefaults = b
	}
	if _, v, ok, err := one("alt", "$alt"); err != nil {
		return p, err
	} else if ok {
		alt, opt, _ := strings.Cut(v, ";")
		switch opt {
		case "":
		case "enum-encoding=int":
			p.EnumInt = true
		default:
			return p, apierror.InvalidArgument("Invalid value for alt: %q", v)
		}
		p.Alt = alt
	}
	_, spec, ok, err := one("fields", "$fields")
	if err != nil {
		return p, err
	}
	if !ok && h != nil {
		spec = strings.Join(h.Values("X-Goog-FieldMask"), ",")
	}
	if p.Fields, err = Parse(spec); err != nil {
		return p, err
	}
	return p, nil
}

func boolOf(name, v string) (bool, error) {
	switch v {
	case "true", "1", "":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, apierror.InvalidArgument("Invalid boolean value for %s: %q", name, v)
}

// ContentTypeJSON is a JSON response's content type.
const ContentTypeJSON = "application/json; charset=UTF-8"

// Body formats a JSON response body under p: pruned to Fields (when prune
// is set), indented when prettyPrint=true and compacted when
// prettyPrint=false, and wrapped in the JSONP callback. It returns the body
// and its content type.
//
// Google indents when the request does not name prettyPrint; CloudBurrow
// leaves the body as the handler wrote it then (compact, for the
// transcoder), which is the same JSON. A front that indents by default sets
// PrettySet first.
func (p Params) Body(b []byte, prune bool) ([]byte, string) { return p.body(b, prune, true) }

// body is Body; keep leaves an unpruned body as the handler formatted it
// unless the request named prettyPrint.
func (p Params) body(b []byte, prune, keep bool) ([]byte, string) {
	if prune && p.Fields != nil {
		if v, err := decode(b); err == nil {
			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			enc.SetEscapeHTML(false)
			if enc.Encode(p.Fields.Prune(v)) == nil {
				b = bytes.TrimRight(out.Bytes(), "\n")
			}
		}
	}
	if p.PrettySet || !keep {
		b = p.format(b)
	}
	if p.Callback != "" {
		return JSONP(p.Callback, b), "text/javascript; charset=UTF-8"
	}
	return b, ContentTypeJSON
}

// format indents b, or compacts it when prettyPrint=false.
func (p Params) format(b []byte) []byte {
	var out bytes.Buffer
	if p.Pretty {
		if json.Indent(&out, b, "", "  ") != nil {
			return b
		}
		return append(bytes.TrimRight(out.Bytes(), "\n"), '\n')
	}
	if json.Compact(&out, b) != nil {
		return b
	}
	return out.Bytes()
}

func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// JSONP wraps a JSON body in a call of callback, as Google's JSONP answers
// are written.
func JSONP(callback string, b []byte) []byte {
	out := make([]byte, 0, len(b)+len(callback)+24)
	out = append(out, "// API callback\n"...)
	out = append(out, callback...)
	out = append(out, '(')
	out = append(out, bytes.TrimRight(b, "\n")...)
	return append(out, ");\n"...)
}

// ErrorBody formats an error body (the v2 envelope, {"error": {"code",
// "message", "status", "details"}}) under p: in the v1 format when
// $.xgafv=1, which adds the legacy errors[] list of {message, domain,
// reason}, then as Body does, unpruned.
func (p Params) ErrorBody(b []byte) ([]byte, string) {
	return p.errorBody(b, true)
}

func (p Params) errorBody(b []byte, keep bool) ([]byte, string) {
	if p.XGAFV == 1 {
		b = toV1(b)
	}
	return p.body(b, false, keep)
}

// v1Reasons are the legacy errors[].reason of each canonical code.
var v1Reasons = map[string]string{
	"INVALID_ARGUMENT": "badRequest", "FAILED_PRECONDITION": "failedPrecondition", "OUT_OF_RANGE": "outOfRange",
	"UNAUTHENTICATED": "authError", "PERMISSION_DENIED": "forbidden", "NOT_FOUND": "notFound",
	"ALREADY_EXISTS": "alreadyExists", "ABORTED": "aborted", "RESOURCE_EXHAUSTED": "rateLimitExceeded",
	"CANCELLED": "cancelled", "DATA_LOSS": "dataLoss", "UNKNOWN": "unknown", "INTERNAL": "backendError",
	"UNIMPLEMENTED": "notImplemented", "UNAVAILABLE": "backendError", "DEADLINE_EXCEEDED": "deadlineExceeded",
}

func toV1(b []byte) []byte {
	var env map[string]map[string]any
	if json.Unmarshal(b, &env) != nil || env["error"] == nil {
		return b
	}
	e := env["error"]
	if _, ok := e["errors"]; ok {
		return b
	}
	st, _ := e["status"].(string)
	reason := v1Reasons[st]
	if reason == "" {
		reason = "unknown"
	}
	e["errors"] = []map[string]any{{"message": e["message"], "domain": "global", "reason": reason}}
	nb, err := json.Marshal(env)
	if err != nil {
		return b
	}
	return nb
}

// WriteError writes err as Google's JSON error under p.
func (p Params) WriteError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	b, ct := p.ErrorBody(apierror.MarshalJSON(e))
	w.Header().Set("Content-Type", ct)
	w.Header().Del("Content-Length")
	w.WriteHeader(e.HTTPStatus())
	_, _ = w.Write(b)
}

// WriteError writes err as Google's JSON error under r's system
// parameters, as far as they parse.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	p, _ := FromRequest(r)
	p.WriteError(w, err)
}
