package aipfilter

import (
	"strconv"

	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Page cuts one page from items, which are already filtered and ordered,
// keeping their order (paging.Page sorts keys, which would undo an
// order_by). The token is an offset bound to scope, which should name the
// collection, the filter and the order_by, so a token is never replayed on
// a different listing. size is clamped as paging.ClampPageSize does.
func Page[T any](scope string, items []T, token string, size int) ([]T, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	start := 0
	if token != "" {
		after, err := paging.DecodeToken(scope, token)
		n, convErr := strconv.Atoi(after)
		if err != nil || convErr != nil || n < 0 || n > len(items) {
			return nil, "", apierror.InvalidArgument("invalid page_token %q", token)
		}
		start = n
	}
	end := start + paging.ClampPageSize(size)
	if end >= len(items) {
		return items[start:], "", nil
	}
	return items[start:end], paging.EncodeToken(scope, strconv.Itoa(end)), nil
}

// ListPage filters, orders and pages items: a list method's whole body
// once it has gathered its resources in their default order.
func ListPage[T proto.Message](scope, filter, orderBy string, items []T, token string, size int, opts ...Option) ([]T, string, error) {
	out, err := List(filter, orderBy, items, opts...)
	if err != nil {
		return nil, "", err
	}
	return Page(scope+"|"+filter+"|"+orderBy, out, token, size)
}
