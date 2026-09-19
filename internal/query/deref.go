package query

import "reflect"

// derefPointer unwraps one level of pointer. Nullable ClickHouse columns
// arrive as pointers to the underlying type, and a nil pointer is an empty
// label rather than an error.
func derefPointer(v any) (any, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer {
		return nil, false
	}
	if rv.IsNil() {
		return nil, true
	}
	return rv.Elem().Interface(), true
}
