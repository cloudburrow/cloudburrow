package logfilter

import (
	"strings"
)

// vkind is a value's type, which a comparison converts its right-hand side to.
type vkind int

const (
	kAny vkind = iota // inferred from the value itself (payloads, labels)
	kString
	kNumber
	kBool
	kTime
	kSeverity
	kDuration
)

// free marks a field whose subfields are not in LogEntry's schema: maps and
// payloads, where an absent key is missing (every comparison on it fails)
// rather than defaulted.
const free vkind = -1

// schema is LogEntry's fields, by path, with their types
// (docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry). A field
// it holds that an entry lacks is defaulted: it compares as its type's zero
// value. A path under a free field is the entry's own, and missing when absent.
var schema = map[string]vkind{
	"logName":                   kString,
	"resource":                  kAny,
	"resource.type":             kString,
	"resource.labels":           free,
	"timestamp":                 kTime,
	"receiveTimestamp":          kTime,
	"severity":                  kSeverity,
	"insertId":                  kString,
	"httpRequest":               kAny,
	"httpRequest.requestMethod": kString,
	"httpRequest.requestUrl":    kString,
	"httpRequest.requestSize":   kNumber,
	"httpRequest.status":        kNumber,
	"httpRequest.responseSize":  kNumber,
	"httpRequest.userAgent":     kString,
	"httpRequest.remoteIp":      kString,
	"httpRequest.serverIp":      kString,
	"httpRequest.referer":       kString,
	"httpRequest.latency":       kDuration,
	"httpRequest.cacheLookup":   kBool,
	"httpRequest.cacheHit":      kBool,
	"httpRequest.cacheValidatedWithOriginServer": kBool,
	"httpRequest.cacheFillBytes":                 kNumber,
	"httpRequest.protocol":                       kString,
	"labels":                                     free,
	"metadata":                                   free,
	"operation":                                  kAny,
	"operation.id":                               kString,
	"operation.producer":                         kString,
	"operation.first":                            kBool,
	"operation.last":                             kBool,
	"trace":                                      kString,
	"spanId":                                     kString,
	"traceSampled":                               kBool,
	"sourceLocation":                             kAny,
	"sourceLocation.file":                        kString,
	"sourceLocation.line":                        kNumber,
	"sourceLocation.function":                    kString,
	"split":                                      kAny,
	"split.uid":                                  kString,
	"split.index":                                kNumber,
	"split.totalSplits":                          kNumber,
	"errorGroups":                                kAny,
	"errorGroups.id":                             kString,
	"apphub":                                     free,
	"apphubDestination":                          free,
	"apphubSource":                               free,
	"textPayload":                                kString,
	"jsonPayload":                                free,
	"protoPayload":                               free,
}

// fieldInfo is what a path names: its LogEntry path (camelCase), its type
// and whether it lies under a free field.
type fieldInfo struct {
	path  []string
	typ   vkind
	free  bool // under a map or payload: missing when absent
	proto bool // under protoPayload: names ignore letter case
}

// camel turns a proto field name (http_request) into its JSON name.
func camel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	up := false
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveField checks a path against LogEntry's schema: an undefined field is
// an error before the query runs, as the language documents.
func resolveField(segs []string) (fieldInfo, *Error) {
	fi := fieldInfo{typ: kAny}
	var known []string
	for _, s := range segs {
		if fi.free {
			fi.path = append(fi.path, s)
			continue
		}
		name := s
		if _, ok := schema[strings.Join(append(known, name), ".")]; !ok {
			name = camel(s)
		}
		key := strings.Join(append(known, name), ".")
		t, ok := schema[key]
		if !ok {
			return fi, errorf("Invalid filter: field %q is not defined in LogEntry (%q is not a field of %s)", strings.Join(segs, "."), s, parentName(known))
		}
		known = append(known, name)
		fi.path = append(fi.path, name)
		if t == free {
			fi.free = true
			fi.proto = name == "protoPayload"
			fi.typ = kAny
			continue
		}
		fi.typ = t
	}
	return fi, nil
}

func parentName(known []string) string {
	if len(known) == 0 {
		return "LogEntry"
	}
	return strings.Join(known, ".")
}
