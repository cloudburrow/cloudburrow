package queryfront

import (
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
)

// GQL key literals in a named database's call (#1885). Google's GQL writes
// a key as KEY([PROJECT(<project>),] [NAMESPACE(<namespace>),] <kind>,
// <id>, ...), and a key with no PROJECT is the query's project
// (https://cloud.google.com/datastore/docs/reference/gql_reference). The
// emulator keeps a named database in a project of its own, so the
// PROJECT of a key literal naming the call's project names the database's
// emulator project; and a DATABASE(<database>) naming the call's database,
// which Google's grammar has not but a client may write, is dropped, the
// emulator project's being its default database. Every other part of the
// query, its string literals, quoted names and bindings included, passes
// as it is; bindings' key values are partition IDs, which mapRequest
// rewrites.

// gqlToken is one lexical token of GQL: start and end offsets, and its kind.
type gqlToken struct {
	start, end int
	kind       byte // 'w' word, 's' string literal, 'q' backquoted name, 'p' punctuation
}

// gqlTokens splits q into tokens, skipping white space. An unterminated
// literal runs to the end of q.
func gqlTokens(q string) []gqlToken {
	var out []gqlToken
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(q) {
				if q[j] == '\\' && j+1 < len(q) {
					j += 2
					continue
				}
				if q[j] == c {
					// A doubled quote is the quote itself.
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			k := byte('s')
			if c == '`' {
				k = 'q'
			}
			out = append(out, gqlToken{i, j, k})
			i = j
		case isGQLWord(c):
			j := i
			for j < len(q) && isGQLWord(q[j]) {
				j++
			}
			out = append(out, gqlToken{i, j, 'w'})
			i = j
		default:
			out = append(out, gqlToken{i, i + 1, 'p'})
			i++
		}
	}
	return out
}

func isGQLWord(c byte) bool {
	return c == '_' || c == '$' || c == '@' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// gqlStringValue is a GQL string literal's value.
func gqlStringValue(lit string) string {
	if len(lit) < 2 {
		return ""
	}
	quote := lit[0]
	body := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\' && i+1 < len(body):
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(body[i])
			}
		case body[i] == quote && i+1 < len(body) && body[i+1] == quote:
			b.WriteByte(quote)
			i++
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}

// rewriteGQLKeys is GQL query q of project's named database with its key
// literals naming the database's emulator project (above).
func rewriteGQLKeys(q, project, database string) string {
	toks := gqlTokens(q)
	type edit struct {
		start, end int
		with       string
	}
	var edits []edit
	// fn(lit) at i: word, "(", string literal, ")".
	call := func(i int, name string) (string, bool) {
		if i+3 >= len(toks) || toks[i].kind != 'w' || !strings.EqualFold(q[toks[i].start:toks[i].end], name) {
			return "", false
		}
		if q[toks[i+1].start:toks[i+1].end] != "(" || toks[i+2].kind != 's' || q[toks[i+3].start:toks[i+3].end] != ")" {
			return "", false
		}
		return gqlStringValue(q[toks[i+2].start:toks[i+2].end]), true
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != 'w' || !strings.EqualFold(q[toks[i].start:toks[i].end], "KEY") || q[toks[i+1].start:toks[i+1].end] != "(" {
			continue
		}
		// The KEY's leading PROJECT, NAMESPACE and DATABASE, in any
		// order, each followed by a comma.
		for j := i + 2; j < len(toks); {
			if v, ok := call(j, "PROJECT"); ok {
				if v == project {
					edits = append(edits, edit{toks[j+2].start, toks[j+2].end, "'" + EmulatorProject(project, database) + "'"})
				}
			} else if v, ok := call(j, "DATABASE"); ok {
				if v == database && j+4 < len(toks) && q[toks[j+4].start:toks[j+4].end] == "," {
					edits = append(edits, edit{toks[j].start, toks[j+4].end, ""})
				}
			} else if _, ok := call(j, "NAMESPACE"); !ok {
				break
			}
			if j+4 >= len(toks) || q[toks[j+4].start:toks[j+4].end] != "," {
				break
			}
			j += 5
		}
	}
	if len(edits) == 0 {
		return q
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(q[last:e.start])
		b.WriteString(e.with)
		last = e.end
	}
	b.WriteString(q[last:])
	return b.String()
}

// mapGQL rewrites the GQL query of a named database's call, if it has one.
func mapGQL(req any, project, database string) {
	var g *pb.GqlQuery
	switch r := req.(type) {
	case *pb.RunQueryRequest:
		g = r.GetGqlQuery()
	case *pb.RunAggregationQueryRequest:
		g = r.GetGqlQuery()
	}
	if g != nil {
		g.QueryString = rewriteGQLKeys(g.QueryString, project, database)
	}
}
