package runtime

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestConfigClassificationCoversEveryDigestedField keeps ADR-0003 honest: every
// configuration leaf that participates in ConfigDigest must be named in its
// inventory. Adding a field without classifying it fails here.
func TestConfigClassificationCoversEveryDigestedField(t *testing.T) {
	adr, err := os.ReadFile("../docs/adr/0003-configuration-authority-classification.md")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	configLeaves(reflect.TypeOf(OperatorConfig{}), "", &paths)
	var repository []string
	configLeaves(reflect.TypeOf(RepositoryConfig{}), "", &repository)
	for _, p := range repository {
		paths = append(paths, ".zenchron.json:"+p)
	}
	for _, p := range paths {
		if !strings.Contains(string(adr), "`"+p+"`") {
			t.Errorf("configuration field %q is not classified in ADR-0003", p)
		}
	}
}

func configLeaves(typ reflect.Type, prefix string, out *[]string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch {
	case typ.Kind() == reflect.Map:
		configLeaves(typ.Elem(), prefix+"<id>.", out)
		return
	case typ.Kind() != reflect.Struct:
		*out = append(*out, strings.TrimSuffix(prefix, "."))
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		configLeaves(typ.Field(i).Type, prefix+name+".", out)
	}
}
