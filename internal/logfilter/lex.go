// Package logfilter is Cloud Logging's query language
// (docs.cloud.google.com/logging/docs/view/logging-query-language): one
// parser and evaluator for the Logging API's entries.list and tail filters,
// sinks, exclusions, log views, log-based metrics, CopyLogEntries and the
// console's Logs Explorer (#2026).
//
//	filter     = expr
//	expr       = orExpr { [ "AND" ] orExpr }      (adjacent terms are ANDed)
//	orExpr     = unary { "OR" unary }             (OR binds tighter than AND)
//	unary      = ( "NOT" | "-" ) unary | primary  (NOT binds tightest)
//	primary    = "(" expr ")" | comparison | boolFunc | value
//	comparison = lhs op rhs | lhs ":*"
//	lhs        = path | cast(...) | regexp_extract(...)
//	op         = "=" | "!=" | "<" | "<=" | ">" | ">=" | ":" | "=~" | "!~"
//	rhs        = value | "(" valueExpr ")"        (values joined by NOT, OR, AND)
//	boolFunc   = SEARCH | log_id | sample | ip_in_net | source
//	path       = segment { "." segment }          (a segment is a word or "quoted")
//
// A bare value is a global restriction: some field of the entry has it.
// AND, OR and NOT are keywords only in upper case; "--" starts a comment.
package logfilter

import (
	"fmt"
	"strings"
)

// MaxLen is the longest query the language accepts, comments included.
const MaxLen = 20000

// Error is a filter the language refuses; Error() is the message an
// INVALID_ARGUMENT carries.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error { return &Error{Msg: fmt.Sprintf(format, args...)} }

type kind int

const (
	tEOF kind = iota
	tWord
	tString
	tOp     // = != < <= > >= : =~ !~
	tLParen // (
	tRParen // )
	tComma
	tStar
	tMinus // a leading "-": NOT, or a number's sign
)

type token struct {
	kind      kind
	text      string // the source text (a string's is unquoted in val)
	val       string
	pos, end  int
	line, col int
}

func (t token) String() string {
	if t.kind == tEOF {
		return "<EOF>"
	}
	return t.text
}

// specials end an unquoted word.
const specials = " \t\r\n\"()<>=!:~,*"

func lex(src string) ([]token, error) {
	var out []token
	line, lineStart := 1, 0
	at := func(kind kind, i, j int, val string) {
		out = append(out, token{kind: kind, text: src[i:j], val: val, pos: i, end: j, line: line, col: i - lineStart + 1})
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			i++
			line, lineStart = line+1, i
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != '"'; j++ {
				if src[j] != '\\' || j+1 >= len(src) {
					b.WriteByte(src[j])
					continue
				}
				j++
				switch src[j] {
				case 't':
					b.WriteByte('\t')
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\':
					b.WriteByte(src[j])
				default:
					// Regular expressions keep their escapes (\d, \.).
					b.WriteByte('\\')
					b.WriteByte(src[j])
				}
			}
			if j >= len(src) {
				return nil, errorf("Unparseable filter: unterminated string at line %d, column %d", line, i-lineStart+1)
			}
			at(tString, i, j+1, b.String())
			i = j + 1
		case c == '(':
			at(tLParen, i, i+1, "(")
			i++
		case c == ')':
			at(tRParen, i, i+1, ")")
			i++
		case c == ',':
			at(tComma, i, i+1, ",")
			i++
		case c == '*':
			at(tStar, i, i+1, "*")
			i++
		case c == '-':
			at(tMinus, i, i+1, "-")
			i++
		case strings.IndexByte("<>=!:~", c) >= 0:
			j := i + 1
			if j < len(src) && (src[j] == '=' || src[j] == '~') && c != ':' && c != '~' {
				j++
			}
			op := src[i:j]
			switch op {
			case "=", "!=", "<", "<=", ">", ">=", ":", "=~", "!~":
			default:
				return nil, syntaxErr(token{kind: tOp, text: op, line: line, col: i - lineStart + 1})
			}
			at(tOp, i, j, op)
			i = j
		default:
			j := i
			for j < len(src) && strings.IndexByte(specials, src[j]) < 0 {
				j++
			}
			at(tWord, i, j, src[i:j])
			i = j
		}
	}
	out = append(out, token{kind: tEOF, pos: len(src), end: len(src), line: line, col: len(src) - lineStart + 1})
	return out, nil
}

// syntaxErr is the message Cloud Logging gives a filter it cannot parse.
func syntaxErr(t token) *Error {
	return errorf("Unparseable filter: syntax error at line %d, column %d, token '%s'", t.line, t.col, t)
}
