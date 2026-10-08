// Package strictjson decodes exactly one JSON value, refusing what
// encoding/json would otherwise accept silently: duplicate object keys,
// unknown struct fields and trailing data.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrTrailingData reports bytes after the single JSON value.
var ErrTrailingData = errors.New("trailing data after JSON value")

// KeyError reports an object key the scan refused.
type KeyError struct {
	Key    string
	Reason string
}

func (e *KeyError) Error() string { return fmt.Sprintf("%s %q", e.Reason, e.Key) }

// Decode decodes data into v, refusing duplicate keys, unknown fields and
// trailing data.
func Decode(data []byte, v any) error { return decode(data, v, false) }

// DecodeCanonical is Decode that also refuses keys that are not lower case:
// encoding/json matches field names case-insensitively, so "Mode" would
// silently override "mode".
func DecodeCanonical(data []byte, v any) error { return decode(data, v, true) }

func decode(data []byte, v any, lowerCase bool) error {
	if err := scanKeys(data, lowerCase); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrTrailingData
	}
	return nil
}

// scanKeys walks the token stream because encoding/json silently keeps the
// last of two duplicate keys, which would let a conflicting field hide behind
// the earlier one a reviewer reads.
func scanKeys(data []byte, lowerCase bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	type frame struct {
		keys    map[string]bool
		object  bool
		wantKey bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '}' || d == ']' {
				stack = stack[:len(stack)-1]
				continue
			}
			if top != nil && top.object {
				top.wantKey = true
			}
			stack = append(stack, &frame{keys: map[string]bool{}, object: d == '{', wantKey: d == '{'})
			continue
		}
		if top == nil || !top.object {
			continue
		}
		if !top.wantKey {
			top.wantKey = true
			continue
		}
		key := tok.(string)
		if lowerCase && strings.ToLower(key) != key {
			return &KeyError{Key: key, Reason: "non-canonical key spelling"}
		}
		if top.keys[key] {
			return &KeyError{Key: key, Reason: "duplicate key"}
		}
		top.keys[key] = true
		top.wantKey = false
	}
}
