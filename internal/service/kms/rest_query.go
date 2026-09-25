package kms

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The one Cloud KMS REST query-parameter policy (#422), shared by every
// transcoded method.
//
// Request fields the path does not bind arrive as query parameters, named by
// field path (http.proto@5b03e5ec:87-88); each method names the ones it
// takes, and both the JSON (camelCase) and proto (snake_case) spellings are
// accepted. Whether Google accepts snake_case is UNVERIFIED.
//
// System parameters (cloud.google.com/apis/docs/system-parameters):
//   - alt / $alt = json is accepted. The GAPIC REST client sends
//     `$alt=json;enum-encoding=int` on every call; enums are then written as
//     numbers. Whether Google does so is UNVERIFIED; both clients read either.
//   - prettyPrint, $prettyPrint and $.xgafv are accepted and ignored: the
//     discovery client and apitools send alt=json&prettyPrint=false.
//   - any other alt, and fields / $fields, are UNIMPLEMENTED, naming the
//     parameter.
//
// Any other parameter is INVALID_ARGUMENT, naming it. That is a KMS choice:
// the Cloud Tasks and Secret Manager JSON APIs ignore unknown parameters, but
// a dropped parameter would look as though it had taken effect. The code
// Google uses is UNVERIFIED.

// query is a request's parsed parameters.
type query struct {
	fields  map[string]string // by JSON name
	enumInt bool              // write enums as numbers
}

// parseQuery applies the policy; fields are the JSON names of the request
// fields this method takes as parameters.
func parseQuery(r *http.Request, fields ...string) (query, error) {
	q := query{fields: map[string]string{}}
	known := map[string]string{}
	for _, f := range fields {
		known[f] = f
		known[snake(f)] = f
	}
	vals, err := rawQuery(r.URL.RawQuery)
	if err != nil {
		return q, err
	}
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := vals[n]
		switch n {
		case "alt", "$alt":
			switch v {
			case "json":
			case "json;enum-encoding=int":
				q.enumInt = true
			default:
				return q, apierror.Unimplemented("the %s=%s system parameter is not implemented: only json is", n, v)
			}
		case "prettyPrint", "$prettyPrint", "$.xgafv":
		case "fields", "$fields":
			return q, apierror.Unimplemented("the %s system parameter is not implemented", n)
		default:
			f, ok := known[n]
			if !ok {
				return q, apierror.InvalidArgument("unknown query parameter %q", n)
			}
			q.fields[f] = v
		}
	}
	return q, nil
}

// rawQuery splits a query on & only. url.ParseQuery drops any pair with a
// raw ";", and `$alt=json;enum-encoding=int` is one; the GAPIC client
// escapes it, but other clients need not. A repeated parameter is refused:
// none of these fields is repeated.
func rawQuery(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			return nil, apierror.InvalidArgument("a query parameter name is not validly escaped")
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			return nil, apierror.InvalidArgument("the %s query parameter is not validly escaped", name)
		}
		if _, dup := out[name]; dup {
			return nil, apierror.InvalidArgument("query parameter %q is repeated", name)
		}
		out[name] = val
	}
	return out, nil
}

// snake is a camelCase JSON name's proto spelling.
func snake(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('_')
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

// int32Field parses an integer parameter; absent is 0.
func (q query) int32Field(name string) (int32, error) {
	v, ok := q.fields[name]
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, apierror.InvalidArgument("%s must be an integer, not %q", name, v)
	}
	return int32(n), nil
}

// boolField parses a boolean parameter, true or false; absent is false.
func (q query) boolField(name string) (bool, error) {
	switch v, ok := q.fields[name]; {
	case !ok || v == "false":
		return false, nil
	case v == "true":
		return true, nil
	default:
		return false, apierror.InvalidArgument("%s must be true or false, not %q", name, v)
	}
}

// viewField parses a CryptoKeyVersionView parameter, by name or number.
func (q query) viewField(name string) (kmspb.CryptoKeyVersion_CryptoKeyVersionView, error) {
	v, ok := q.fields[name]
	if !ok {
		return 0, nil
	}
	if n, ok := kmspb.CryptoKeyVersion_CryptoKeyVersionView_value[v]; ok {
		return kmspb.CryptoKeyVersion_CryptoKeyVersionView(n), nil
	}
	if n, err := strconv.ParseInt(v, 10, 32); err == nil {
		return kmspb.CryptoKeyVersion_CryptoKeyVersionView(n), nil
	}
	return 0, apierror.InvalidArgument("%s is not a CryptoKeyVersionView: %q", name, v)
}

// marshal is the protojson form of a response under this query.
func (q query) marshal() protojson.MarshalOptions {
	return protojson.MarshalOptions{UseEnumNumbers: q.enumInt}
}
