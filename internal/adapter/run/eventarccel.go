package run

// The CEL of Eventarc Advanced (#1947), as Google's "Transform received
// events" page documents it: an enrollment's cel_match and a pipeline's
// transformation and message binding templates read the message, a map of
// its CloudEvents context attributes and its data, as `message` (and, in
// a binding, `headers`, the headers earlier mediations added). The
// extension functions are the page's: setField, removeFields, merge,
// denormalize, toMap, toJsonString, toBase64, toDestinationPayloadFormat,
// toCloudEventJsonWithPayloadFormat, and re.capture, re.captureN and
// re.extract (RE2).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// eaFormatter encodes a message's data in a destination's payload
// format, for toDestinationPayloadFormat; nil leaves it as it is.
type eaFormatter func(data any) (any, error)

var eaMapType = cel.MapType(cel.StringType, cel.DynType)

// eaEnv is the CEL environment of an expression; format is the
// destination's encoder, for the binding functions.
func eaEnv(binding bool, format eaFormatter) (*cel.Env, error) {
	native := func(v ref.Val) any { return eaNative(v) }
	val := func(x any) ref.Val { return types.DefaultTypeAdapter.NativeToValue(x) }
	mapArg := func(v ref.Val) (map[string]any, bool) {
		m, ok := native(v).(map[string]any)
		return m, ok
	}
	opts := []cel.EnvOption{
		cel.Variable("message", eaMapType),
		cel.Function("setField", cel.MemberOverload("map_setField", []*cel.Type{eaMapType, cel.StringType, cel.DynType}, eaMapType,
			cel.FunctionBinding(func(args ...ref.Val) ref.Val {
				m, ok := mapArg(args[0])
				if !ok {
					return types.NewErr("setField: the receiver is not a map")
				}
				eaSetPath(m, strings.Split(string(args[1].(types.String)), "."), native(args[2]))
				return val(m)
			}))),
		cel.Function("removeFields", cel.MemberOverload("map_removeFields", []*cel.Type{eaMapType, cel.ListType(cel.StringType)}, eaMapType,
			cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
				m, ok := mapArg(lhs)
				if !ok {
					return types.NewErr("removeFields: the receiver is not a map")
				}
				paths, _ := native(rhs).([]any)
				for _, p := range paths {
					eaRemovePath(m, strings.Split(fmt.Sprint(p), "."))
				}
				return val(m)
			}))),
		cel.Function("merge", cel.MemberOverload("map_merge", []*cel.Type{eaMapType, eaMapType}, eaMapType,
			cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
				a, ok1 := mapArg(lhs)
				b, ok2 := mapArg(rhs)
				if !ok1 || !ok2 {
					return types.NewErr("merge: both sides must be maps")
				}
				return val(eaMerge(a, b))
			}))),
		cel.Function("denormalize",
			cel.MemberOverload("map_denormalize", []*cel.Type{eaMapType}, eaMapType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return val(eaDenormalize(native(v))) })),
			cel.MemberOverload("list_denormalize", []*cel.Type{cel.ListType(cel.DynType)}, eaMapType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return val(eaDenormalize(native(v))) }))),
		cel.Function("toMap", cel.MemberOverload("list_toMap", []*cel.Type{cel.ListType(cel.DynType)}, eaMapType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				out := map[string]any{}
				l, _ := native(v).([]any)
				for _, x := range l {
					if m, ok := x.(map[string]any); ok {
						out = eaMerge(out, m)
					}
				}
				return val(out)
			}))),
		cel.Function("toJsonString", cel.MemberOverload("dyn_toJsonString", []*cel.Type{cel.DynType}, cel.StringType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				b, err := json.Marshal(native(v))
				if err != nil {
					return types.NewErr("toJsonString: %v", err)
				}
				return types.String(b)
			}))),
		cel.Function("toBase64", cel.MemberOverload("dyn_toBase64", []*cel.Type{cel.DynType}, cel.StringType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				var b []byte
				switch x := native(v).(type) {
				case []byte:
					b = x
				case string:
					b = []byte(x)
				default:
					b, _ = json.Marshal(x)
				}
				return types.String(base64.URLEncoding.EncodeToString(b))
			}))),
		cel.Function("re.capture", cel.Overload("re_capture", []*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
			cel.BinaryBinding(func(t, r ref.Val) ref.Val {
				re, err := regexp.Compile(string(r.(types.String)))
				if err != nil {
					return types.NewErr("re.capture: %v", err)
				}
				m := re.FindStringSubmatch(string(t.(types.String)))
				if len(m) < 2 {
					return types.NewErr("re.capture: no captured group matched")
				}
				return types.String(m[1])
			}))),
		cel.Function("re.captureN", cel.Overload("re_captureN", []*cel.Type{cel.StringType, cel.StringType}, eaMapType,
			cel.BinaryBinding(func(t, r ref.Val) ref.Val {
				re, err := regexp.Compile(string(r.(types.String)))
				if err != nil {
					return types.NewErr("re.captureN: %v", err)
				}
				s := string(t.(types.String))
				loc := re.FindStringSubmatchIndex(s)
				if loc == nil || loc[0] != 0 || loc[1] != len(s) {
					return types.NewErr("re.captureN: the regular expression does not match the whole string")
				}
				out := map[string]any{}
				for i, name := range re.SubexpNames() {
					if i == 0 || loc[2*i] < 0 {
						continue
					}
					if name == "" {
						name = strconv.Itoa(i)
					}
					out[name] = s[loc[2*i]:loc[2*i+1]]
				}
				return val(out)
			}))),
		cel.Function("re.extract", cel.Overload("re_extract", []*cel.Type{cel.StringType, cel.StringType, cel.StringType}, cel.StringType,
			cel.FunctionBinding(func(args ...ref.Val) ref.Val {
				re, err := regexp.Compile(string(args[1].(types.String)))
				if err != nil {
					return types.NewErr("re.extract: %v", err)
				}
				s := string(args[0].(types.String))
				m := re.FindStringSubmatchIndex(s)
				if m == nil {
					return types.NewErr("re.extract: the regular expression does not match")
				}
				// RE2's \1 rewrite groups, as Go's ${1}.
				tmpl := regexp.MustCompile(`\\(\d)`).ReplaceAllString(string(args[2].(types.String)), `$${$1}`)
				return types.String(re.ExpandString(nil, tmpl, s, m))
			}))),
	}
	if binding {
		opts = append(opts, cel.Variable("headers", eaMapType),
			cel.Function("toDestinationPayloadFormat", cel.MemberOverload("dyn_toDestinationPayloadFormat", []*cel.Type{cel.DynType}, cel.DynType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					if format == nil {
						return v
					}
					out, err := format(native(v))
					if err != nil {
						return types.NewErr("toDestinationPayloadFormat: %v", err)
					}
					return val(out)
				}))),
			cel.Function("toCloudEventJsonWithPayloadFormat", cel.MemberOverload("map_toCloudEventJson", []*cel.Type{eaMapType}, eaMapType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					m, ok := mapArg(v)
					if !ok {
						return types.NewErr("toCloudEventJsonWithPayloadFormat: the receiver is not a map")
					}
					for _, a := range []string{"id", "source", "specversion", "type"} {
						if _, ok := m[a]; !ok {
							return types.NewErr("toCloudEventJsonWithPayloadFormat: the message has no %q attribute", a)
						}
					}
					if format != nil && m["data"] != nil {
						d, err := format(m["data"])
						if err != nil {
							return types.NewErr("toCloudEventJsonWithPayloadFormat: %v", err)
						}
						if b, ok := d.([]byte); ok {
							delete(m, "data")
							m["data_base64"] = base64.StdEncoding.EncodeToString(b)
						} else {
							m["data"] = d
						}
					}
					return val(m)
				}))))
	}
	return cel.NewEnv(opts...)
}

// eaCompileMatch compiles an enrollment's match, which must be a bool.
func eaCompileMatch(expr string) (cel.Program, error) {
	env, err := eaEnv(false, nil)
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("the expression's type is %s, not bool", ast.OutputType())
	}
	return env.Program(ast)
}

// eaCompileTemplate compiles a transformation or binding template.
func eaCompileTemplate(expr string, binding bool) (cel.Program, error) {
	return eaCompileTemplateFor(expr, binding, nil)
}

func eaCompileTemplateFor(expr string, binding bool, format eaFormatter) (cel.Program, error) {
	env, err := eaEnv(binding, format)
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	return env.Program(ast)
}

// eaEvalMatch reports whether a message matches; an error is no match.
func eaEvalMatch(expr string, message map[string]any) (bool, error) {
	prg, err := eaCompileMatch(expr)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(map[string]any{"message": message})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("the match is %T, not bool", out.Value())
	}
	return b, nil
}

// eaEvalTemplate runs a template and returns the map it builds.
func eaEvalTemplate(expr string, binding bool, format eaFormatter, vars map[string]any) (map[string]any, error) {
	prg, err := eaCompileTemplateFor(expr, binding, format)
	if err != nil {
		return nil, err
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return nil, err
	}
	m, ok := eaNative(out).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the template's result is %T, not a map", eaNative(out))
	}
	return m, nil
}

// eaNative is a CEL value as Go's JSON-like values.
func eaNative(v ref.Val) any {
	switch x := v.(type) {
	case types.Null:
		return nil
	case types.Bytes:
		return []byte(x)
	case types.Timestamp:
		return x.Time.UTC().Format(time.RFC3339Nano)
	case types.Duration:
		return x.Duration.String()
	case traits.Mapper:
		out := map[string]any{}
		for it := x.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			out[fmt.Sprint(eaNative(k))] = eaNative(x.Get(k))
		}
		return out
	case traits.Lister:
		n, _ := x.Size().Value().(int64)
		out := make([]any, 0, n)
		for i := int64(0); i < n; i++ {
			out = append(out, eaNative(x.Get(types.Int(i))))
		}
		return out
	}
	return v.Value()
}

func eaSetPath(m map[string]any, path []string, v any) {
	for len(path) > 1 {
		next, ok := m[path[0]].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[path[0]] = next
		}
		m, path = next, path[1:]
	}
	m[path[0]] = v
}

func eaRemovePath(m map[string]any, path []string) {
	for len(path) > 1 {
		next, ok := m[path[0]].(map[string]any)
		if !ok {
			return
		}
		m, path = next, path[1:]
	}
	delete(m, path[0])
}

// eaMerge merges b into a: nested maps merge, b's other values win.
func eaMerge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if am, ok := out[k].(map[string]any); ok {
			if bm, ok := v.(map[string]any); ok {
				out[k] = eaMerge(am, bm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// eaDenormalize flattens a map or list into period-delimited keys.
func eaDenormalize(v any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		join := func(k string) string {
			if prefix == "" {
				return k
			}
			return prefix + "." + k
		}
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(join(k), x[k])
			}
		case []any:
			for i, e := range x {
				walk(join(strconv.Itoa(i)), e)
			}
		default:
			out[prefix] = v
		}
	}
	walk("", v)
	return out
}
