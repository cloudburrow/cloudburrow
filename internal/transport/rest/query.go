package rest

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The Transcoder's query-parameter policy, first written for Cloud KMS
// (#422) and shared by every transcoded method.
//
// Request fields the path and body do not bind arrive as query parameters,
// named by field path (http.proto@5b03e5ec:87-88), in either the JSON
// (camelCase) or the proto (snake_case) spelling of each segment. Whether
// Google accepts snake_case is UNVERIFIED. A leaf is a scalar, an enum (by
// name or number), a repeated one of those (the parameter repeated), or a
// well-known type with a string JSON form (FieldMask, Timestamp, Duration,
// the wrappers).
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
// Any other parameter is INVALID_ARGUMENT, naming it: a dropped parameter
// would look as though it had taken effect. The Cloud Tasks and Secret
// Manager routers ignore unknown parameters instead; the code Google uses is
// UNVERIFIED.

// query is a request's parsed system parameters.
type query struct {
	enumInt bool // write enums as numbers
}

// marshal is the protojson form of a response under this query.
func (q query) marshal() protojson.MarshalOptions {
	return protojson.MarshalOptions{UseEnumNumbers: q.enumInt}
}

// param is a query parameter resolved to the field it sets.
type param struct {
	name   string // the canonical JSON field path, for errors
	values []string
	fd     protoreflect.FieldDescriptor
	path   []protoreflect.FieldDescriptor // from the request to fd
}

// parseQuery applies the policy to raw, setting the fields it names on req.
// Every name is checked before any value is parsed, so an unknown parameter
// is reported ahead of a malformed one.
func parseQuery(raw string, req protoreflect.Message, rt *Route) (query, error) {
	var q query
	vals, err := rawQuery(raw)
	if err != nil {
		return q, err
	}
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	var params []param
	set := map[protoreflect.FieldDescriptor]string{}
	for _, n := range names {
		vs := vals[n]
		switch n {
		case "alt", "$alt", "prettyPrint", "$prettyPrint", "$.xgafv", "fields", "$fields":
			if len(vs) > 1 {
				return q, apierror.InvalidArgument("query parameter %q is repeated", n)
			}
		}
		switch n {
		case "alt", "$alt":
			switch vs[0] {
			case "json":
			case "json;enum-encoding=int":
				q.enumInt = true
			default:
				return q, apierror.Unimplemented("the %s=%s system parameter is not implemented: only json is", n, vs[0])
			}
		case "prettyPrint", "$prettyPrint", "$.xgafv":
		case "fields", "$fields":
			return q, apierror.Unimplemented("the %s system parameter is not implemented", n)
		default:
			p, ok := queryField(req.Descriptor(), rt, n)
			if !ok {
				return q, apierror.InvalidArgument("unknown query parameter %q", n)
			}
			// Only a repeated field takes a parameter twice, in either
			// spelling.
			if _, dup := set[p.fd]; (dup || len(vs) > 1) && !p.fd.IsList() {
				return q, apierror.InvalidArgument("query parameter %q is repeated", n)
			}
			set[p.fd] = n
			p.values = vs
			params = append(params, p)
		}
	}
	for _, p := range params {
		m := req
		for _, fd := range p.path[:len(p.path)-1] {
			m = m.Mutable(fd).Message()
		}
		if p.fd.IsList() {
			list := m.Mutable(p.fd).List()
			for _, v := range p.values {
				val, err := scalar(p.fd, p.name, v, list.NewElement)
				if err != nil {
					return q, err
				}
				list.Append(val)
			}
			continue
		}
		fd := p.fd
		val, err := scalar(fd, p.name, p.values[0], func() protoreflect.Value { return m.NewField(fd) })
		if err != nil {
			return q, err
		}
		m.Set(p.fd, val)
	}
	return q, nil
}

// queryField resolves a parameter name to a request field the route leaves
// to the query: not bound by the path, not in the body.
func queryField(md protoreflect.MessageDescriptor, rt *Route, name string) (param, bool) {
	if rt.Body == "*" {
		return param{}, false
	}
	p := param{}
	var jsonPath, protoPath []string
	segs := strings.Split(name, ".")
	for i, seg := range segs {
		if md == nil {
			return param{}, false
		}
		fd := md.Fields().ByJSONName(seg)
		if fd == nil {
			fd = md.Fields().ByName(protoreflect.Name(seg))
		}
		if fd == nil || fd.IsMap() {
			return param{}, false
		}
		p.path = append(p.path, fd)
		jsonPath = append(jsonPath, fd.JSONName())
		protoPath = append(protoPath, string(fd.Name()))
		prefix := strings.Join(protoPath, ".")
		if prefix == rt.Body {
			return param{}, false
		}
		for _, v := range rt.vars {
			if prefix == v {
				return param{}, false
			}
		}
		last := i == len(segs)-1
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			if last {
				if !stringForm[fd.Message().FullName()] {
					return param{}, false
				}
			} else if fd.IsList() {
				return param{}, false
			}
			md = fd.Message()
		} else {
			md = nil
		}
		if last {
			p.fd = fd
		}
	}
	p.name = strings.Join(jsonPath, ".")
	return p, p.fd != nil
}

// stringForm are the well-known types whose JSON form is a string or a
// number, and so can be a query parameter.
var stringForm = map[protoreflect.FullName]bool{
	"google.protobuf.FieldMask": true, "google.protobuf.Timestamp": true, "google.protobuf.Duration": true,
	"google.protobuf.StringValue": true, "google.protobuf.BytesValue": true, "google.protobuf.BoolValue": true,
	"google.protobuf.Int32Value": true, "google.protobuf.Int64Value": true, "google.protobuf.UInt32Value": true,
	"google.protobuf.UInt64Value": true, "google.protobuf.FloatValue": true, "google.protobuf.DoubleValue": true,
}

// scalar parses v, the text of a path variable or query parameter, as fd's
// type; name is how an error names it. newMsg makes the message a
// well-known-type field holds.
func scalar(fd protoreflect.FieldDescriptor, name, v string, newMsg func() protoreflect.Value) (protoreflect.Value, error) {
	integer := func(bits int, signed bool) (protoreflect.Value, error) {
		if signed {
			n, err := strconv.ParseInt(v, 10, bits)
			if err != nil {
				return protoreflect.Value{}, apierror.InvalidArgument("%s must be an integer, not %q", name, v)
			}
			if bits == 32 {
				return protoreflect.ValueOfInt32(int32(n)), nil
			}
			return protoreflect.ValueOfInt64(n), nil
		}
		n, err := strconv.ParseUint(v, 10, bits)
		if err != nil {
			return protoreflect.Value{}, apierror.InvalidArgument("%s must be a non-negative integer, not %q", name, v)
		}
		if bits == 32 {
			return protoreflect.ValueOfUint32(uint32(n)), nil
		}
		return protoreflect.ValueOfUint64(n), nil
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v), nil
	case protoreflect.BoolKind:
		switch v {
		case "true":
			return protoreflect.ValueOfBool(true), nil
		case "false":
			return protoreflect.ValueOfBool(false), nil
		}
		return protoreflect.Value{}, apierror.InvalidArgument("%s must be true or false, not %q", name, v)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return integer(32, true)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return integer(64, true)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return integer(32, false)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return integer(64, false)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		bits := 64
		if fd.Kind() == protoreflect.FloatKind {
			bits = 32
		}
		f, err := strconv.ParseFloat(v, bits)
		if err != nil {
			return protoreflect.Value{}, apierror.InvalidArgument("%s must be a number, not %q", name, v)
		}
		if bits == 32 {
			return protoreflect.ValueOfFloat32(float32(f)), nil
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.BytesKind:
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(v); err == nil {
				return protoreflect.ValueOfBytes(b), nil
			}
		}
		return protoreflect.Value{}, apierror.InvalidArgument("%s is not valid base64", name)
	case protoreflect.EnumKind:
		ed := fd.Enum()
		if ev := ed.Values().ByName(protoreflect.Name(v)); ev != nil {
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
		}
		return protoreflect.Value{}, apierror.InvalidArgument("%s is not a %s: %q", name, ed.Name(), v)
	case protoreflect.MessageKind:
		if !stringForm[fd.Message().FullName()] {
			break
		}
		// FieldMask's JSON form is lowerCamelCase paths, which protojson
		// turns into proto (snake_case) paths, so a server's mask checks
		// see one form.
		val := newMsg()
		quoted, _ := json.Marshal(v)
		if err := protojson.Unmarshal(quoted, val.Message().Interface()); err != nil {
			if fd.Message().FullName() == "google.protobuf.FieldMask" {
				return protoreflect.Value{}, apierror.InvalidArgument("%s is not a field mask of lowerCamelCase paths: %q", name, v)
			}
			return protoreflect.Value{}, apierror.InvalidArgument("%s is not a valid %s: %q", name, fd.Message().Name(), v)
		}
		return val, nil
	}
	return protoreflect.Value{}, apierror.InvalidArgument("%s cannot be set from the path or query", name)
}

// rawQuery splits a query on & only. url.ParseQuery drops any pair with a
// raw ";", and `$alt=json;enum-encoding=int` is one; the GAPIC client
// escapes it, but other clients need not.
func rawQuery(raw string) (map[string][]string, error) {
	out := map[string][]string{}
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
		out[name] = append(out[name], val)
	}
	return out, nil
}
