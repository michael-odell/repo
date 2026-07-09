package config

import "fmt"

// StringList decodes a TOML value that may be either a single string or a list
// of strings, so `apply_to = "*"` and `apply_to = ["work", "vendor"]` both work.
type StringList []string

// UnmarshalTOML implements toml.Unmarshaler.
func (s *StringList) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*s = StringList{x}
	case []any:
		out := make(StringList, 0, len(x))
		for _, e := range x {
			str, ok := e.(string)
			if !ok {
				return fmt.Errorf("apply_to: expected string, got %T", e)
			}
			out = append(out, str)
		}
		*s = out
	default:
		return fmt.Errorf("apply_to: expected string or list of strings, got %T", v)
	}
	return nil
}
