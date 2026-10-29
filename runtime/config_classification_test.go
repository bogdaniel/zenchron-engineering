package runtime

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestConfigClassificationCoversEveryDigestedField keeps ADR-0003 honest: every
// configuration leaf that participates in ConfigDigest must be named in its §3
// inventory tables. Adding a field without classifying it fails here; a mention
// elsewhere in the ADR (such as the #328 discussion in §4) does not count.
func TestConfigClassificationCoversEveryDigestedField(t *testing.T) {
	adr, err := os.ReadFile("../docs/adr/0003-configuration-authority-classification.md")
	if err != nil {
		t.Fatal(err)
	}
	_, inventory, found := strings.Cut(string(adr), "### 3. Field inventory")
	inventory, _, ended := strings.Cut(inventory, "### 4.")
	if !found || !ended {
		t.Fatal("ADR-0003 has no §3 inventory section")
	}
	var paths []string
	configLeaves(reflect.TypeOf(OperatorConfig{}), "", &paths)
	var repository []string
	configLeaves(reflect.TypeOf(RepositoryConfig{}), "", &repository)
	for _, p := range repository {
		paths = append(paths, ".zenchron.json:"+p)
	}
	for _, p := range paths {
		if !strings.Contains(inventory, "`"+p+"`") {
			t.Errorf("configuration field %q is not classified in ADR-0003 §3", p)
		}
	}
}

// configLeaves lists the JSON paths encoding/json would emit for typ: tagged
// fields by tag, untagged exported fields by Go name, and untagged embedded
// structs flattened into their parent.
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
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				configLeaves(embedded, prefix, out)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		configLeaves(field.Type, prefix+name+".", out)
	}
}
