package fieldpath

import "strings"

// Resolve extracts a string value from a nested map using a dot-notation path.
// Path format: ".spec.vpcRef.name" (leading dot is optional).
func Resolve(obj map[string]interface{}, path string) string {
	path = strings.TrimPrefix(path, ".")
	parts := strings.Split(path, ".")

	current := obj
	for i, part := range parts {
		if i == len(parts)-1 {
			val, ok := current[part]
			if !ok {
				return ""
			}
			s, ok := val.(string)
			if !ok {
				return ""
			}
			return s
		}
		next, ok := current[part]
		if !ok {
			return ""
		}
		m, ok := next.(map[string]interface{})
		if !ok {
			return ""
		}
		current = m
	}
	return ""
}
