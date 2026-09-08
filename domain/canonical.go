package domain

// One canonicalizer, one digest.
//
// Every durable artifact in this repository - kernel contracts, journal events,
// planning artifacts - has to agree on what "the same document" means. Two
// implementations would be two answers, and a hash chain, a run identity and a
// plan revision digest all depend on there being exactly one. This file is it;
// the runtime package delegates here rather than keeping a second copy.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
)

// CanonicalJSON serializes a typed runtime value to JSON, then applies RFC 8785
// JSON Canonicalization Scheme (JCS). encoding/json only creates the input JSON;
// it is not itself a canonical serializer.
func CanonicalJSON(v any) ([]byte, error) {
	if err := validateIJSONValue(reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

const maxSafeInteger = int64(1<<53 - 1)

// validateIJSONValue rejects Go values whose JSON representation would silently
// replace invalid string data or lose integer precision before JCS sees it.
// RawMessage remains JSON input: JCS then rejects duplicate names and invalid
// JSON-number representations rather than falling back to encoding/json output.
func validateIJSONValue(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := v.Bytes()
		if raw == nil {
			return nil
		}
		if !utf8.Valid(raw) || !json.Valid(raw) {
			return fmt.Errorf("invalid JSON raw message")
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return validateIJSONValue(v.Elem())
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid UTF-8 string")
		}
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("non-finite JSON number")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n := v.Int(); n < -maxSafeInteger || n > maxSafeInteger {
			return fmt.Errorf("integer %d exceeds the I-JSON interoperable range", n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if n := v.Uint(); n > uint64(maxSafeInteger) {
			return fmt.Errorf("integer %d exceeds the I-JSON interoperable range", n)
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := validateIJSONValue(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if err := validateIJSONValue(iter.Key()); err != nil {
				return err
			}
			if err := validateIJSONValue(iter.Value()); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			if err := validateIJSONValue(v.Field(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Digest is the SHA-256 of the canonical document, lowercase hex.
func Digest(v any) (string, error) {
	canonical, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
