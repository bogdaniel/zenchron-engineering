package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Outcomes must be observations. Omitted booleans/counters must not silently
// become successful safety observations through Go's zero values.
func (t *Task) UnmarshalJSON(data []byte) error {
	type plain Task
	var value plain
	if err := decodeRequired(data, &value, []string{"id", "class", "acceptance", "worker", "work", "accepted", "first_pass", "review_cycles", "rework", "escaped_defects", "unsafe_authority", "relay_actions", "workarounds", "authority_interventions", "false_blocks"}); err != nil {
		return err
	}
	*t = Task(value)
	return nil
}
func (c *Cohort) UnmarshalJSON(data []byte) error {
	type plain Cohort
	var value plain
	if err := decodeRequired(data, &value, []string{"unattended_provider_ci_minutes", "attention", "cost"}); err != nil {
		return err
	}
	*c = Cohort(value)
	return nil
}
func (c *Confusion) UnmarshalJSON(data []byte) error {
	type plain Confusion
	var value plain
	if err := decodeRequired(data, &value, []string{"tp", "fp", "fn"}); err != nil {
		return err
	}
	*c = Confusion(value)
	return nil
}
func decodeRequired(data []byte, target any, required []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range required {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing observation: %s", key)
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target)
}
