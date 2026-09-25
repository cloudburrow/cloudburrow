package storage

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// objects.list (#494): prefix, delimiter (synthetic prefixes, which count
// toward maxResults), includeTrailingDelimiter, startOffset and endOffset,
// matchGlob, and pages of at most 1000 in lexicographic byte order.

// listToken is where a page resumes: after this name, and, when it was a
// synthetic prefix, after everything under it.
type listToken struct {
	After  string `json:"a"`
	Prefix bool   `json:"p,omitempty"`
}

func (s *Server) objectsList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	for _, p := range []struct{ name, issue string }{{"versions", "#498"}, {"softDeleted", "#499"}} {
		if q.Get(p.name) == "true" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s=true is not implemented yet (%s)", p.name, p.issue))
			return
		}
	}
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
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	trailing := q.Get("includeTrailingDelimiter") == "true"
	start, end := q.Get("startOffset"), q.Get("endOffset")
	var glob *regexp.Regexp
	if g := q.Get("matchGlob"); g != "" {
		re, err := globRE(g)
		if err != nil {
			writeError(w, err)
			return
		}
		glob = re
	}

	var items []any
	var prefixes []string
	seen := map[string]bool{}
	var last listToken
	next := ""
	err := s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		base := objectPrefix + bucket + "/"
		for _, key := range tx.List(base + prefix) {
			name := strings.TrimPrefix(key, base)
			if tok.After != "" && (name <= tok.After || tok.Prefix && strings.HasPrefix(name, tok.After)) {
				continue
			}
			if start != "" && name < start || end != "" && name >= end {
				continue
			}
			if glob != nil && !glob.MatchString(name) {
				continue
			}
			// A delimiter after the prefix folds the name into a synthetic
			// prefix; with includeTrailingDelimiter, an object whose name is
			// exactly that prefix is listed too.
			var p string
			if delim != "" {
				if i := strings.Index(name[len(prefix):], delim); i >= 0 {
					p = name[:len(prefix)+i+len(delim)]
				}
			}
			emitPrefix := p != "" && !seen[p]
			emitItem := p == "" || trailing && name == p
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
			if emitItem {
				o, ok, err := getObject(tx, bucket, name)
				if err != nil {
					return err
				}
				if ok {
					items = append(items, s.objectJSON(r, o))
				}
				if !emitPrefix {
					last = listToken{After: name}
				}
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
