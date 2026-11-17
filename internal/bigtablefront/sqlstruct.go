package bigtablefront

import (
	"fmt"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// STRUCT constructors (#1704), as GoogleSQL for Bigtable's data types
// reference documents them
// (https://cloud.google.com/bigtable/docs/reference/sql/data-types#constructing_a_struct):
//
//	(expr1, expr2 [, ...])                       tuple syntax, anonymous fields
//	STRUCT(expr1 [AS field_name] [, ...])        typeless; a column names its field
//	STRUCT<[field_name] field_type, ...>(expr1 [, ...])  typed, no AS
//
// A typed constructor's expressions are coerced to the field types: an
// INT64 to a FLOAT, a literal to the type it can be read as; any other
// mismatch is an error.

type structLit struct {
	elems []expr
	names []string
	typed *btpb.Type // the typed syntax's type
	t     *btpb.Type
}

func (s *structLit) check(sc *scope) (*btpb.Type, error) {
	var names []string
	var types []*btpb.Type
	for i, e := range s.elems {
		t, err := e.check(sc)
		if err != nil {
			return nil, err
		}
		if s.typed != nil {
			want := s.typed.GetStructType().GetFields()[i].GetType()
			if !sameType(t, want) {
				l, isLit := e.(*literal)
				switch {
				case isLit && l.t == nil:
				case isKind(t, "INT64") && isKind(want, "FLOAT32", "FLOAT64"),
					isLit && isKind(t, "STRING") && isKind(want, "DATE", "TIMESTAMP", "BYTES"):
					c := &cast{x: e, to: kind(want)}
					if _, err := c.check(sc); err != nil {
						return nil, err
					}
					s.elems[i] = c
				default:
					return nil, fmt.Errorf("Struct field %d has type %s which does not coerce to %s", i+1, kind(t), kind(want))
				}
			}
			continue
		}
		names = append(names, s.names[i])
		types = append(types, t)
	}
	if s.typed != nil {
		s.t = s.typed
	} else {
		s.t = structType(names, types)
	}
	return s.t, nil
}

func (s *structLit) eval(e *env) (any, error) {
	out := make([]any, len(s.elems))
	for i, x := range s.elems {
		v, err := x.eval(e)
		if err != nil {
			return nil, err
		}
		out[i] = coerce(v, s.t.GetStructType().GetFields()[i].GetType())
	}
	return out, nil
}

func (s *structLit) String() string {
	var p []string
	for i, x := range s.elems {
		p = append(p, x.String()+" AS "+s.names[i])
	}
	return "STRUCT(" + strings.Join(p, ",") + ")"
}

// structCtor reads a STRUCT constructor, after the STRUCT keyword.
func (p *parser) structCtor() (expr, error) {
	s := &structLit{}
	if p.op("<") {
		var names []string
		var types []*btpb.Type
		if !p.op(">") {
			for {
				name := ""
				// A field name is an identifier followed by its type.
				if t := p.peek(); (t.kind == "ident" || t.kind == "qident") && (p.peekAt(1).kind == "ident") {
					name = t.text
					p.pos++
				}
				tn, err := p.typeName()
				if err != nil {
					return nil, err
				}
				ft, err := parseTypeName(tn)
				if err != nil {
					return nil, err
				}
				names = append(names, name)
				types = append(types, ft)
				if p.op(",") {
					continue
				}
				if p.op(">>") {
					return nil, fmt.Errorf("Syntax error: unexpected >> in STRUCT type")
				}
				if err := p.expect(">"); err != nil {
					return nil, err
				}
				break
			}
		}
		s.typed = structType(names, types)
		s.names = names
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	if p.op(")") {
		if s.typed != nil && len(s.names) != 0 {
			return nil, fmt.Errorf("STRUCT type has %d fields but constructor call has 0 fields", len(s.names))
		}
		return s, nil
	}
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		name := ""
		if p.keyword("AS") {
			if s.typed != nil {
				return nil, fmt.Errorf("STRUCT constructors cannot specify both an explicit type and field names with AS")
			}
			t := p.next()
			if t.kind != "ident" && t.kind != "qident" {
				return nil, fmt.Errorf("Syntax error: expected a field name after AS")
			}
			name = t.text
		} else if c, ok := e.(*colRef); ok {
			name = c.name // STRUCT(1, t.str_col) is STRUCT<int64, str_col string>
		} else if f, ok := e.(*field); ok {
			name = f.name
		}
		s.elems = append(s.elems, e)
		if s.typed == nil {
			s.names = append(s.names, name)
		}
		if !p.op(",") {
			break
		}
	}
	if s.typed != nil && len(s.elems) != len(s.names) {
		return nil, fmt.Errorf("STRUCT type has %d fields but constructor call has %d fields", len(s.names), len(s.elems))
	}
	return s, p.expect(")")
}
