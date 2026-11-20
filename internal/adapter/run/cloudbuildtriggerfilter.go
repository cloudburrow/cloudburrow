package run

import (
	"path"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// A trigger's event filters (#1955): its includedFiles and ignoredFiles,
// and its CEL filter, as Google's BuildTrigger reference and "Automate
// builds in response to Pub/Sub events" document them.
//
// includedFiles and ignoredFiles are "file glob matches using
// https://golang.org/pkg/path/filepath/#Match extended with support for
// **": a path segment ** matches any number of segments, none included.
// They filter a repository push (a trigger on a Developer Connect link to
// a Secure Source Manager repository, cloudbuilddevconnect.go) by its
// changed files: with neither set, every push builds; otherwise the files
// matching no ignoredFiles glob are kept, a push with none kept does not
// build, and with includedFiles set at least one kept file must match one
// of its globs. A manual run (RunBuildTrigger) is not filtered. On a
// trigger with no repository events they are refused UNIMPLEMENTED, as
// there are no changed files to match.
//
// filter is "a Common Expression Language string" over the trigger's
// substitution variables, which must evaluate to true for an event's
// build to run: the trigger's own (_ACTION, bound from a Pub/Sub or
// webhook payload) and the built-ins a repository push has (BRANCH_NAME,
// TAG_NAME, REF_NAME, COMMIT_SHA, SHORT_SHA, REVISION_ID, REPO_NAME,
// REPO_FULL_NAME, TRIGGER_NAME, PROJECT_ID; "" where an event has none).
// ==, !=, matches(), && and || are documented; "the ! (not) symbol is not
// supported", so a filter using it is INVALID_ARGUMENT, as is one that
// does not compile (an unknown variable) or is not boolean. A manual run
// is not filtered.

// triggerFilterBuiltins are the built-in substitutions a filter can name.
var triggerFilterBuiltins = []string{"BRANCH_NAME", "TAG_NAME", "REF_NAME", "COMMIT_SHA", "SHORT_SHA", "REVISION_ID",
	"REPO_NAME", "REPO_FULL_NAME", "TRIGGER_NAME", "PROJECT_ID"}

var triggerFilterPrograms = struct {
	mu sync.Mutex
	m  map[string]cel.Program // by the variables and the expression
}{m: map[string]cel.Program{}}

// triggerFilterVars are the variables a trigger's filter can name.
func triggerFilterVars(t *cbpb.BuildTrigger) []string {
	vars := append([]string(nil), triggerFilterBuiltins...)
	for _, k := range sortedKeys(t.GetSubstitutions()) {
		vars = append(vars, k)
	}
	return vars
}

// compileTriggerFilter compiles a trigger's filter, or refuses it.
func compileTriggerFilter(t *cbpb.BuildTrigger) (cel.Program, error) {
	src := t.GetFilter()
	vars := triggerFilterVars(t)
	key := strings.Join(vars, ",") + "\x00" + src
	triggerFilterPrograms.mu.Lock()
	prg, ok := triggerFilterPrograms.m[key]
	triggerFilterPrograms.mu.Unlock()
	if ok {
		return prg, nil
	}
	opts := make([]cel.EnvOption, 0, len(vars))
	for _, v := range vars {
		opts = append(opts, cel.Variable(v, cel.StringType))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, apierror.Internal(err, "the CEL environment")
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, apierror.InvalidArgument("invalid trigger: filter %q is not a valid CEL expression over the trigger's substitutions: %v", src, iss.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, apierror.InvalidArgument("invalid trigger: filter %q must evaluate to a bool, not %s", src, ast.OutputType())
	}
	checked, err := cel.AstToCheckedExpr(ast)
	if err != nil {
		return nil, apierror.Internal(err, "the filter's checked expression")
	}
	if usesNot(checked.GetExpr()) {
		return nil, apierror.InvalidArgument("invalid trigger: filter %q uses the ! (not) operator, which trigger filters do not support", src)
	}
	prg, err = env.Program(ast)
	if err != nil {
		return nil, apierror.InvalidArgument("invalid trigger: filter %q: %v", src, err)
	}
	triggerFilterPrograms.mu.Lock()
	triggerFilterPrograms.m[key] = prg
	triggerFilterPrograms.mu.Unlock()
	return prg, nil
}

// usesNot reports whether an expression applies the logical not.
func usesNot(e *exprpb.Expr) bool {
	if e == nil {
		return false
	}
	switch k := e.GetExprKind().(type) {
	case *exprpb.Expr_CallExpr:
		if k.CallExpr.GetFunction() == "!_" {
			return true
		}
		if usesNot(k.CallExpr.GetTarget()) {
			return true
		}
		for _, a := range k.CallExpr.GetArgs() {
			if usesNot(a) {
				return true
			}
		}
	case *exprpb.Expr_SelectExpr:
		return usesNot(k.SelectExpr.GetOperand())
	case *exprpb.Expr_ListExpr:
		for _, x := range k.ListExpr.GetElements() {
			if usesNot(x) {
				return true
			}
		}
	case *exprpb.Expr_StructExpr:
		for _, x := range k.StructExpr.GetEntries() {
			if usesNot(x.GetMapKey()) || usesNot(x.GetValue()) {
				return true
			}
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := k.ComprehensionExpr
		for _, x := range []*exprpb.Expr{c.GetIterRange(), c.GetAccuInit(), c.GetLoopCondition(), c.GetLoopStep(), c.GetResult()} {
			if usesNot(x) {
				return true
			}
		}
	}
	return false
}

// triggerFilterAccepts reports whether an event, with its substitution
// values, passes a trigger's filter; true when it has none.
func triggerFilterAccepts(t *cbpb.BuildTrigger, values map[string]string) (bool, error) {
	if t.GetFilter() == "" {
		return true, nil
	}
	prg, err := compileTriggerFilter(t)
	if err != nil {
		return false, err
	}
	act := map[string]any{}
	for _, v := range triggerFilterVars(t) {
		act[v] = values[v]
	}
	out, _, err := prg.Eval(act)
	if err != nil {
		return false, nil // an evaluation error (a bad regex) builds nothing
	}
	b, ok := out.Value().(bool)
	return ok && b, nil
}

// checkFileGlobs refuses a malformed includedFiles or ignoredFiles glob.
func checkFileGlobs(field string, globs []string) error {
	for _, g := range globs {
		if g == "" {
			return apierror.InvalidArgument("invalid trigger: %s has an empty glob", field)
		}
		for _, seg := range strings.Split(g, "/") {
			if _, err := path.Match(seg, ""); err != nil {
				return apierror.InvalidArgument("invalid trigger: %s glob %q: %v", field, g, err)
			}
		}
	}
	return nil
}

// fileGlobMatch matches a file path against a glob: path.Match per
// segment, with a ** segment matching any number of segments.
func fileGlobMatch(glob, name string) bool {
	return globSegments(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func globSegments(g, n []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for len(g) > 0 && g[0] == "**" {
				g = g[1:]
			}
			if len(g) == 0 {
				return true
			}
			for i := 0; i <= len(n); i++ {
				if globSegments(g, n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], n[0]); !ok {
			return false
		}
		g, n = g[1:], n[1:]
	}
	return len(n) == 0
}

// anyGlob reports whether a file matches one of the globs.
func anyGlob(globs []string, name string) bool {
	for _, g := range globs {
		if fileGlobMatch(g, name) {
			return true
		}
	}
	return false
}

// triggerFilesAccept reports whether a push's changed files pass a
// trigger's includedFiles and ignoredFiles, as BuildTrigger documents.
func triggerFilesAccept(t *cbpb.BuildTrigger, files []string) bool {
	inc, ign := t.GetIncludedFiles(), t.GetIgnoredFiles()
	if len(inc) == 0 && len(ign) == 0 {
		return true
	}
	for _, f := range files {
		if anyGlob(ign, f) {
			continue
		}
		if len(inc) == 0 || anyGlob(inc, f) {
			return true
		}
	}
	return false
}

// unsupportedTriggerFilters checks a trigger's filter and file globs.
func unsupportedTriggerFilters(t *cbpb.BuildTrigger) error {
	if len(t.GetIncludedFiles()) > 0 || len(t.GetIgnoredFiles()) > 0 {
		if !onDevConnectLink(t) {
			return apierror.Unimplemented("BuildTrigger fields includedFiles and ignoredFiles filter a repository push by its changed files: " +
				"they are supported on a trigger whose repositoryEventConfig (or developerConnectEventConfig) names a Developer Connect link " +
				"to a Secure Source Manager repository, not on this trigger, which has no changed files")
		}
		if err := checkFileGlobs("includedFiles", t.GetIncludedFiles()); err != nil {
			return err
		}
		if err := checkFileGlobs("ignoredFiles", t.GetIgnoredFiles()); err != nil {
			return err
		}
	}
	if t.GetFilter() != "" {
		if _, err := compileTriggerFilter(t); err != nil {
			return err
		}
	}
	return nil
}

// filterValues are the substitution values a filter sees for a build.
func filterValues(project string, b *cbpb.Build, builtins map[string]string) map[string]string {
	out := map[string]string{"PROJECT_ID": project}
	for k, v := range b.GetSubstitutions() {
		out[k] = v
	}
	for k, v := range builtins {
		out[k] = v
	}
	return out
}
