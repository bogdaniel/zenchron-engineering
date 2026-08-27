package domain

import (
	"encoding/json"
	"fmt"
)

// FactValue preserves true, false, and explicit unknown as distinct states.
// Its zero value is invalid and therefore cannot represent field absence.
type FactValue string

const (
	FactTrue    FactValue = "true"
	FactFalse   FactValue = "false"
	FactUnknown FactValue = "unknown"
)

// MarshalJSON preserves booleans as JSON booleans and unknown as a string.
func (v FactValue) MarshalJSON() ([]byte, error) {
	switch v {
	case FactTrue:
		return []byte("true"), nil
	case FactFalse:
		return []byte("false"), nil
	case FactUnknown:
		return []byte(`"unknown"`), nil
	default:
		return nil, fmt.Errorf("invalid fact value %q", v)
	}
}

// UnmarshalJSON accepts only the three v0.1 fact values.
func (v *FactValue) UnmarshalJSON(data []byte) error {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	switch decoded := decoded.(type) {
	case bool:
		if decoded {
			*v = FactTrue
		} else {
			*v = FactFalse
		}
	case string:
		if decoded != "unknown" {
			return fmt.Errorf("invalid fact value %q", decoded)
		}
		*v = FactUnknown
	default:
		return fmt.Errorf("invalid fact value %s", data)
	}
	return nil
}
