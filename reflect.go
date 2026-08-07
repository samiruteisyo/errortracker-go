package errortracker

import "reflect"

// reflectTypeName renders an error's concrete type, e.g. "*net.OpError".
//
// The type name is a fingerprint component, so its stability matters more than
// its prettiness: two occurrences of the same failure must produce the same
// string. reflect gives that; fmt's %T formatting of a wrapped error does not
// always.
func reflectTypeName(v any) string {
	if v == nil {
		return ""
	}
	t := reflect.TypeOf(v)
	if t == nil {
		return ""
	}
	// Pointer types render as *pkg.Type, which is what Go itself prints and
	// what anyone reading the dashboard will recognise.
	if t.Kind() == reflect.Ptr && t.Elem().PkgPath() != "" {
		return "*" + t.Elem().String()
	}
	return t.String()
}
