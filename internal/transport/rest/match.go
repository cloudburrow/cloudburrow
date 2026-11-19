package rest

import (
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Matcher names the gRPC method an HTTP request is bound to by the
// methods' google.api.http rules, and the resource fields its path sets,
// without serving it: what IAM enforcement (#1879) reads to check a JSON
// request as it checks the same call over gRPC.
type Matcher struct {
	routes []Route
}

// NewMatcher binds the methods of the full gRPC service names given, and
// the mixin rules (the IAMPolicy methods a service's yaml binds).
func NewMatcher(services []string, mixins []*annotations.HttpRule) *Matcher {
	m := &Matcher{}
	for _, s := range services {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(s))
		if err != nil {
			continue
		}
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if !ok {
			continue
		}
		for j := 0; j < sd.Methods().Len(); j++ {
			md := sd.Methods().Get(j)
			if rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule); rule != nil {
				m.routes = append(m.routes, routesOf(string(sd.FullName()), string(md.Name()), rule)...)
			}
		}
	}
	for _, r := range mixins {
		i := strings.LastIndexByte(r.GetSelector(), '.')
		if i > 0 {
			m.routes = append(m.routes, routesOf(r.GetSelector()[:i], r.GetSelector()[i+1:], r)...)
		}
	}
	for i := range m.routes {
		m.routes[i].re, m.routes[i].vars = compileTemplate(m.routes[i].Pattern)
	}
	return m
}

// Match is the full gRPC method ("/pkg.Service/Method") r is bound to and
// the field paths its path sets, by their unescaped values; ok is false
// when no rule binds it.
func (m *Matcher) Match(r *http.Request) (fullMethod string, fields map[string]string, ok bool) {
	path := r.URL.EscapedPath()
	for _, p := range []string{path, strings.ReplaceAll(path, ":", "%3A")} {
		for i := range m.routes {
			rt := &m.routes[i]
			if rt.Method != r.Method {
				continue
			}
			caps := rt.re.FindStringSubmatch(p)
			if caps == nil {
				continue
			}
			fields = map[string]string{}
			for k, v := range rt.vars {
				if u, err := url.PathUnescape(caps[k+1]); err == nil {
					fields[v] = u
				} else {
					fields[v] = caps[k+1]
				}
			}
			return "/" + rt.Service + "/" + rt.MethodName, fields, true
		}
	}
	return "", nil, false
}
