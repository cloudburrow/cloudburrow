package networkservices

import (
	"net"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/parser"
)

// Secure Web Proxy's rule matchers (#2160), as
// https://docs.cloud.google.com/secure-web-proxy/docs/cel-matcher-language-reference
// describes them (read 2026-11-22): a sessionMatcher reads source.ip,
// source.port, destination.port, host(), source.matchTag(),
// source.matchServiceAccount(), inUrlList() and inIpRange(); an
// applicationMatcher reads those and the request attributes
// (request.headers, .method, .host, .path, .query, .scheme, request.url(),
// request.useragent()). A missing map key makes a rule not match.
//
// The per-request functions (host(), request.url(), request.useragent(),
// source.matchTag(), source.matchServiceAccount()) are rewritten by macros
// into hidden variables, so one compiled program serves every request.

const (
	varHost = "__swp_host"
	varURL  = "__swp_url"
	varUA   = "__swp_ua"
	varTags = "__swp_tags"
	varSAs  = "__swp_sas"
)

func identIs(e ast.Expr, name string) bool {
	return e != nil && e.Kind() == ast.IdentKind && e.AsIdent() == name
}

var swpMacros = []cel.Macro{
	cel.GlobalMacro("host", 0, func(eh parser.ExprHelper, _ ast.Expr, _ []ast.Expr) (ast.Expr, *common.Error) {
		return eh.NewIdent(varHost), nil
	}),
	cel.ReceiverMacro("host", 0, func(eh parser.ExprHelper, t ast.Expr, _ []ast.Expr) (ast.Expr, *common.Error) {
		if !identIs(t, "request") {
			return nil, nil
		}
		return eh.NewSelect(t, "host"), nil
	}),
	cel.ReceiverMacro("url", 0, func(eh parser.ExprHelper, t ast.Expr, _ []ast.Expr) (ast.Expr, *common.Error) {
		if !identIs(t, "request") {
			return nil, nil
		}
		return eh.NewIdent(varURL), nil
	}),
	cel.ReceiverMacro("useragent", 0, func(eh parser.ExprHelper, t ast.Expr, _ []ast.Expr) (ast.Expr, *common.Error) {
		if !identIs(t, "request") {
			return nil, nil
		}
		return eh.NewIdent(varUA), nil
	}),
	cel.ReceiverMacro("matchTag", 1, func(eh parser.ExprHelper, t ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
		if !identIs(t, "source") {
			return nil, nil
		}
		return eh.NewCall(operators.In, args[0], eh.NewIdent(varTags)), nil
	}),
	cel.ReceiverMacro("matchServiceAccount", 1, func(eh parser.ExprHelper, t ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
		if !identIs(t, "source") {
			return nil, nil
		}
		return eh.NewCall(operators.In, args[0], eh.NewIdent(varSAs)), nil
	}),
}

// matcherEnv is the CEL environment of one Server's matchers: inUrlList
// reads the server's URL lists through lists.
func matcherEnv(lists func(name string) ([]string, bool), application bool) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("source", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("destination", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable(varHost, cel.StringType),
		cel.Variable(varTags, cel.ListType(cel.StringType)),
		cel.Variable(varSAs, cel.ListType(cel.StringType)),
		cel.Macros(swpMacros...),
		cel.Function("inIpRange", cel.Overload("inIpRange_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(func(a, b ref.Val) ref.Val {
				ip := net.ParseIP(string(a.(types.String)))
				_, n, err := net.ParseCIDR(string(b.(types.String)))
				if err != nil {
					return types.NewErr("inIpRange: invalid range %q", string(b.(types.String)))
				}
				return types.Bool(ip != nil && n.Contains(ip))
			}))),
		cel.Function("inUrlList", cel.Overload("inUrlList_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(func(a, b ref.Val) ref.Val {
				l, ok := lists(relRef(string(b.(types.String))))
				if !ok {
					return types.NewErr("inUrlList: no URL list %q", string(b.(types.String)))
				}
				return types.Bool(urlListMatches(string(a.(types.String)), l))
			}))),
	}
	if application {
		opts = append(opts, cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable(varURL, cel.StringType), cel.Variable(varUA, cel.StringType))
	}
	return cel.NewEnv(opts...)
}

// matchers compiles and caches a Server's matcher programs.
type matchers struct {
	once     sync.Once
	sess     *cel.Env
	app      *cel.Env
	err      error
	programs sync.Map // "s:"/"a:" + expression -> cel.Program
}

func (m *matchers) init(lists func(string) ([]string, bool)) {
	m.once.Do(func() {
		m.sess, m.err = matcherEnv(lists, false)
		if m.err == nil {
			m.app, m.err = matcherEnv(lists, true)
		}
	})
}

// compile checks an expression as a sessionMatcher (application false) or
// applicationMatcher.
func (m *matchers) compile(lists func(string) ([]string, bool), expr string, application bool) (cel.Program, error) {
	m.init(lists)
	if m.err != nil {
		return nil, m.err
	}
	key := "s:" + expr
	env := m.sess
	if application {
		key, env = "a:"+expr, m.app
	}
	if p, ok := m.programs.Load(key); ok {
		return p.(cel.Program), nil
	}
	a, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if t := a.OutputType(); t != cel.BoolType && t != cel.DynType {
		return nil, errNotBool
	}
	p, err := env.Program(a)
	if err != nil {
		return nil, err
	}
	m.programs.Store(key, p)
	return p, nil
}

type celErr string

func (e celErr) Error() string { return string(e) }

const errNotBool = celErr("the expression must evaluate to a boolean")

// CheckMatcher compiles a sessionMatcher (application false) or an
// applicationMatcher, as Network Security checks a rule's expressions.
func CheckMatcher(expr string, application bool) error {
	var m matchers
	_, err := m.compile(func(string) ([]string, bool) { return nil, true }, expr, application)
	return err
}

// urlListMatches is whether value (a host, or a URL without its scheme:
// host + path) is in a URL list, as
// https://docs.cloud.google.com/secure-web-proxy/docs/url-list-syntax-reference
// describes it: "*" is a prefix wildcard of whole subdomain tokens or of
// a path; an entry without a path matches any path; a value without "/"
// (host()) matches only the entries' domains.
func urlListMatches(value string, entries []string) bool {
	vHost, vPath, hasSlash := strings.Cut(value, "/")
	vHost = strings.ToLower(vHost)
	if h, _, err := net.SplitHostPort(vHost); err == nil {
		vHost = h
	}
	vPath = "/" + vPath
	for _, e := range entries {
		e = strings.TrimPrefix(strings.TrimPrefix(e, "https://"), "http://")
		eHost, ePath, eSlash := strings.Cut(e, "/")
		if !hostPatternMatches(strings.ToLower(eHost), vHost) {
			continue
		}
		if !hasSlash || !eSlash {
			return true
		}
		ePath = "/" + ePath
		if strings.HasSuffix(ePath, "*") {
			if strings.HasPrefix(vPath, strings.TrimSuffix(ePath, "*")) {
				return true
			}
			continue
		}
		if vPath == ePath {
			return true
		}
	}
	return false
}

func hostPatternMatches(p, h string) bool {
	switch {
	case strings.HasPrefix(p, "*."):
		return strings.HasSuffix(h, p[1:])
	case strings.HasPrefix(p, "*"):
		return h == p[1:] || strings.HasSuffix(h, "."+p[1:])
	}
	return h == p
}

// ValidURLListEntry is whether an entry is valid: "Entries that contain a
// period (.) and a forward-slash (/) must be well-formed URLs"; patterns
// with a period and no slash are domains; any other entry is invalid.
func ValidURLListEntry(e string) bool {
	if !strings.Contains(e, ".") || strings.ContainsAny(e, " \t\r\n") {
		return false
	}
	if !strings.Contains(e, "/") {
		return true
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(e, "https://"), "http://"), "/")
	return host != "" && !strings.Contains(strings.TrimPrefix(host, "*"), "*")
}
