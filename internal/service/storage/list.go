package storage

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// objects.list (#494): prefix, delimiter (synthetic prefixes, which count
// toward maxResults), includeTrailingDelimiter, startOffset and endOffset,
// matchGlob, and pages of at most 1000 in lexicographic byte order. With
// versions=true (#498) every version is listed, each name's oldest first,
// and names that have only noncurrent versions count.

// listToken is where a page resumes: after this name, and, when it was a
// synthetic prefix, after everything under it. With versions=true, Gen is
// the last version of After listed, and the page resumes after it.
type listToken struct {
	After  string `json:"a"`
	Prefix bool   `json:"p,omitempty"`
	Gen    int64  `json:"g,omitempty"`
}

func (s *Server) objectsList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	mode := listLive
	switch {
	case q.Get("softDeleted") == "true":
		mode = listSoftDeleted
	case q.Get("versions") == "true":
		mode = listAllVersions
	}
	versions := mode != listLive
	max := 1000
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: maxResults=%q", v))
			return
		}
		if n > 0 && n < max {
			max = n
		}
	}
	var tok listToken
	if v := q.Get("pageToken"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || json.Unmarshal(b, &tok) != nil {
			writeError(w, badRequest("Invalid argument: pageToken"))
			return
		}
	}
	sel, err := parseNameSelector(q)
	if err != nil {
		writeError(w, err)
		return
	}
	prefix := sel.Prefix

	filter, err := parseContextFilter(q.Get("filter"))
	if err != nil {
		writeError(w, err)
		return
	}

	var items []any
	var prefixes []string
	seen := map[string]bool{}
	var last listToken
	next := ""
	err = s.meta.View(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		for _, name := range listNames(tx, bucket, prefix, mode) {
			resume := tok.After != "" && name == tok.After && tok.Gen > 0
			if tok.After != "" && !resume && (name <= tok.After || tok.Prefix && strings.HasPrefix(name, tok.After)) {
				continue
			}
			if !sel.inRange(name) {
				continue
			}
			p := sel.fold(name)
			emitPrefix := p != "" && !seen[p]
			emitItem := sel.itemOf(name, p)
			var vs []objectRecord
			if emitItem {
				var err error
				if vs, err = s.listVersions(tx, b, name, mode); err != nil {
					return err
				}
				if resume {
					for len(vs) > 0 && vs[0].Generation <= tok.Gen {
						vs = vs[1:]
					}
				}
				// The filter applies to objects; prefixes are exempt.
				if filter != nil {
					kept := vs[:0:0]
					for _, v := range vs {
						if filter.match(v) {
							kept = append(kept, v)
						}
					}
					vs = kept
				}
				emitItem = len(vs) > 0
			}
			if !emitPrefix && !emitItem {
				continue
			}
			need := 0
			if emitPrefix {
				need++
			}
			if emitItem {
				need++
			}
			if len(items)+len(prefixes)+need > max {
				t, _ := json.Marshal(last)
				next = base64.RawURLEncoding.EncodeToString(t)
				break
			}
			if emitPrefix {
				seen[p] = true
				prefixes = append(prefixes, p)
				last = listToken{After: p, Prefix: true}
			}
			full := false
			for i, v := range vs {
				if i > 0 && len(items)+len(prefixes) >= max {
					full = true
					break
				}
				items = append(items, s.objectJSONWith(r, v, b))
				if !emitPrefix {
					last = listToken{After: name}
					if versions {
						last.Gen = v.Generation
					}
				}
			}
			if full {
				t, _ := json.Marshal(last)
				next = base64.RawURLEncoding.EncodeToString(t)
				break
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		resp["items"] = items
	}
	if len(prefixes) > 0 {
		resp["prefixes"] = prefixes
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}

// nameSelector is the part of an objects.list query that selects names:
// prefix, startOffset and endOffset, matchGlob, and delimiter with
// includeTrailingDelimiter. objects.watchAll takes the same parameters and
// delivers the changes of the objects a listing with them would show
// (#1393), so both read them through this.
type nameSelector struct {
	Prefix    string `json:"prefix,omitempty"`
	Delimiter string `json:"delimiter,omitempty"`
	Trailing  bool   `json:"includeTrailingDelimiter,omitempty"`
	Start     string `json:"startOffset,omitempty"`
	End       string `json:"endOffset,omitempty"`
	Glob      string `json:"matchGlob,omitempty"`
	glob      *regexp.Regexp
}

// parseNameSelector reads q's selecting parameters; a malformed matchGlob
// is a 400.
func parseNameSelector(q url.Values) (nameSelector, error) {
	sel := nameSelector{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		Trailing: q.Get("includeTrailingDelimiter") == "true",
		Start:    q.Get("startOffset"), End: q.Get("endOffset"), Glob: q.Get("matchGlob")}
	return sel, sel.compile()
}

func (sel *nameSelector) compile() error {
	if sel.Glob == "" || sel.glob != nil {
		return nil
	}
	re, err := globRE(sel.Glob)
	if err != nil {
		return err
	}
	sel.glob = re
	return nil
}

// inRange reports whether name is past startOffset, before endOffset and
// matches matchGlob. It does not look at the prefix.
func (sel nameSelector) inRange(name string) bool {
	if sel.Start != "" && name < sel.Start || sel.End != "" && name >= sel.End {
		return false
	}
	return sel.glob == nil || sel.glob.MatchString(name)
}

// fold is the synthetic prefix a delimiter after the prefix folds name
// into, or "" when it folds into none. name must have the prefix.
func (sel nameSelector) fold(name string) string {
	if sel.Delimiter == "" {
		return ""
	}
	if i := strings.Index(name[len(sel.Prefix):], sel.Delimiter); i >= 0 {
		return name[:len(sel.Prefix)+i+len(sel.Delimiter)]
	}
	return ""
}

// itemOf reports whether name, folded into p, is listed as an object: when
// it folds into no prefix, or, with includeTrailingDelimiter, when its name
// is exactly that prefix.
func (sel nameSelector) itemOf(name, p string) bool {
	return p == "" || sel.Trailing && name == p
}

// selects reports whether a listing with sel lists name as an object.
func (sel nameSelector) selects(name string) bool {
	if !strings.HasPrefix(name, sel.Prefix) || !sel.inRange(name) {
		return false
	}
	return sel.itemOf(name, sel.fold(name))
}

// listMode is what a listing shows: live versions, every version
// (versions=true, #498), or only soft-deleted ones (softDeleted=true, #499).
type listMode int

const (
	listLive listMode = iota
	listAllVersions
	listSoftDeleted
)

// listNames returns the names under prefix that mode may show, sorted.
func listNames(tx Tx, bucket, prefix string, mode listMode) []string {
	keys := func(p string) []string {
		base := p + bucket + "/"
		var out []string
		for _, k := range tx.List(base + prefix) {
			out = append(out, strings.TrimPrefix(k, base))
		}
		return out
	}
	switch mode {
	case listSoftDeleted:
		return keys(softObjectPrefix)
	case listLive:
		return keys(objectPrefix)
	}
	names := keys(objectPrefix)
	nbase := noncurrentPrefix + bucket + "/"
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, k := range tx.List(nbase + prefix) {
		if n := strings.TrimPrefix(k, nbase); !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// listVersions returns what a listing in mode shows of one name, oldest
// first.
func (s *Server) listVersions(tx Tx, b bucketRecord, name string, mode listMode) ([]objectRecord, error) {
	bucket := b.Name
	if mode == listSoftDeleted {
		return softVersions(tx, b, name, s.now())
	}
	var vs []objectRecord
	if mode == listAllVersions {
		var err error
		if vs, err = getNoncurrent(tx, bucket, name); err != nil {
			return nil, err
		}
	}
	o, ok, err := getObject(tx, bucket, name)
	if err != nil {
		return nil, err
	}
	if ok {
		vs = append(vs, o)
	}
	return vs, nil
}

// globRE compiles matchGlob's syntax
// (docs.cloud.google.com/storage/docs/json_api/v1/objects/list#list-objects-and-prefixes-using-glob):
// ** matches any characters including "/", * any except "/", ? one character
// except "/", [abc] and [a-z] a class ([!x] negated), and {a,b} alternatives.
func globRE(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				return nil, badRequest("Invalid argument: matchGlob %q has an unclosed [", g)
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case c == '{':
			depth++
			b.WriteString("(?:")
		case c == '}' && depth > 0:
			depth--
			b.WriteString(")")
		case c == ',' && depth > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if depth != 0 {
		return nil, badRequest("Invalid argument: matchGlob %q has an unclosed {", g)
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, badRequest("Invalid argument: matchGlob %q", g)
	}
	return re, nil
}
