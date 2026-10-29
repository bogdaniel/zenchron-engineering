package strictjson

import (
	"errors"
	"testing"
)

func TestDecodeRefusesAmbiguousJSON(t *testing.T) {
	type doc struct {
		Mode string `json:"mode"`
		Sub  struct {
			ID string `json:"id"`
		} `json:"sub"`
	}
	for name, tc := range map[string]struct {
		in        string
		canonical bool
		key       string // want a KeyError on this key
		trailing  bool
		ok        bool
	}{
		"ok":                  {in: `{"mode":"a","sub":{"id":"x"}}`, ok: true},
		"duplicate":           {in: `{"mode":"a","mode":"b"}`, key: "mode"},
		"nested duplicate":    {in: `{"sub":{"id":"x","id":"y"}}`, key: "id"},
		"unknown field":       {in: `{"other":1}`},
		"trailing":            {in: `{"mode":"a"} {}`, trailing: true},
		"case variant":        {in: `{"Mode":"a"}`, ok: true},
		"case variant strict": {in: `{"Mode":"a"}`, canonical: true, key: "Mode"},
	} {
		var v doc
		decode := Decode
		if tc.canonical {
			decode = DecodeCanonical
		}
		err := decode([]byte(tc.in), &v)
		var ke *KeyError
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: unexpected %v", name, err)
		case tc.key != "" && (!errors.As(err, &ke) || ke.Key != tc.key):
			t.Errorf("%s: want key error on %q, got %v", name, tc.key, err)
		case tc.trailing && !errors.Is(err, ErrTrailingData):
			t.Errorf("%s: want trailing-data error, got %v", name, err)
		case !tc.ok && err == nil:
			t.Errorf("%s: accepted", name)
		}
	}
}
