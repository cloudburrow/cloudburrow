package aipfilter

import (
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Order is a compiled AIP-132 order_by: a comma-separated list of field
// paths, each optionally followed by " desc" (or " asc"). A nil *Order keeps
// the list's own order.
type Order struct {
	src  string
	keys []orderKey
}

type orderKey struct {
	steps []step
	desc  bool
}

// String is the order_by's source text.
func (o *Order) String() string {
	if o == nil {
		return ""
	}
	return o.src
}

// CompileOrder parses orderBy for resources of message type md.
func CompileOrder(orderBy string, md protoreflect.MessageDescriptor, opts ...Option) (*Order, error) {
	if strings.TrimSpace(orderBy) == "" {
		return nil, nil
	}
	cfg := newConfig(opts)
	o := &Order{src: orderBy}
	for _, part := range strings.Split(orderBy, ",") {
		words := strings.Fields(part)
		if len(words) == 0 || len(words) > 2 {
			return nil, apierror.InvalidArgument("invalid order_by %q: each item is a field name, optionally followed by \"desc\"", orderBy)
		}
		k := orderKey{}
		if len(words) == 2 {
			switch strings.ToLower(words[1]) {
			case "desc":
				k.desc = true
			case "asc":
			default:
				return nil, apierror.InvalidArgument("invalid order_by %q: %q is not \"asc\" or \"desc\"", orderBy, words[1])
			}
		}
		name := words[0]
		if !cfg.allowed(name) {
			return nil, apierror.InvalidArgument("invalid order_by %q: field %q is not a field results can be ordered by", orderBy, name)
		}
		steps, err := resolvePath(md, cfg.resolve(name), false)
		if err != nil {
			return nil, apierror.InvalidArgument("invalid order_by %q: %v", orderBy, err)
		}
		last := steps[len(steps)-1]
		fd := last.fd
		if last.isKey {
			fd = fd.MapValue()
		} else if fd.IsList() {
			return nil, apierror.InvalidArgument("invalid order_by %q: field %q is repeated", orderBy, name)
		}
		for _, s := range steps[:len(steps)-1] {
			if s.fd.IsList() {
				return nil, apierror.InvalidArgument("invalid order_by %q: field %q passes through a repeated field", orderBy, name)
			}
		}
		if kindOf(fd) == kMessage {
			return nil, apierror.InvalidArgument("invalid order_by %q: field %q is a message; order by one of its fields", orderBy, name)
		}
		k.steps = steps
		o.keys = append(o.keys, k)
	}
	return o, nil
}

// Less compares a and b by the order's keys.
func (o *Order) Less(a, b proto.Message) bool {
	return o.cmp(a.ProtoReflect(), b.ProtoReflect()) < 0
}

func (o *Order) cmp(a, b protoreflect.Message) int {
	for _, k := range o.keys {
		c := cmpLeaf(collect(a, k.steps), collect(b, k.steps), k.steps[len(k.steps)-1])
		if k.desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func cmpLeaf(la, lb []leaf, last step) int {
	// An unset value (a missing map key, an unset message) sorts first.
	if len(la) == 0 || len(lb) == 0 {
		return sign(len(la) - len(lb))
	}
	a, b := la[0], lb[0]
	if a.set != b.set {
		if !a.set {
			return -1
		}
		return 1
	}
	fd := a.fd
	if fd == nil {
		fd = last.fd
	}
	sa, na, ba, ta, da := scalar(a, fd)
	sb, nb, bb, tb, db := scalar(b, fd)
	switch kindOf(fd) {
	case kString:
		return strings.Compare(sa, sb)
	case kEnum, kNumber:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case kBool:
		switch {
		case ba == bb:
			return 0
		case !ba:
			return -1
		}
		return 1
	case kTime:
		return ta.Compare(tb)
	case kDuration:
		return sign(int(da - db))
	}
	return 0
}

// Sort orders items stably by the order's keys; a nil order leaves them.
func Sort[T proto.Message](o *Order, items []T) {
	if o == nil {
		return
	}
	sort.SliceStable(items, func(i, j int) bool { return o.Less(items[i], items[j]) })
}

// SortBy compiles orderBy for T's message type and sorts items by it.
func SortBy[T proto.Message](orderBy string, items []T, opts ...Option) error {
	var zero T
	o, err := CompileOrder(orderBy, zero.ProtoReflect().Descriptor(), opts...)
	if err != nil {
		return err
	}
	Sort(o, items)
	return nil
}

// List filters then orders items: the usual body of a list method.
func List[T proto.Message](filter, orderBy string, items []T, opts ...Option) ([]T, error) {
	out, err := Apply(filter, items, opts...)
	if err != nil {
		return nil, err
	}
	if err := SortBy(orderBy, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}
