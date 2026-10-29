package api_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestSecretNeverPrintsOrSerializes(t *testing.T) {
	const value = "sk-live-do-not-leak"
	holder := struct {
		Name   string
		Secret api.Secret
		Ptr    *api.Secret
	}{Name: "binding", Secret: api.NewSecret(value)}
	holder.Ptr = &holder.Secret

	encoded, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", holder),
		"%+v":  fmt.Sprintf("%+v", holder),
		"%#v":  fmt.Sprintf("%#v", holder),
		"%s":   fmt.Sprintf("%s", holder.Secret),
		"json": string(encoded),
	}
	for verb, out := range outputs {
		if strings.Contains(out, value) {
			t.Errorf("%s leaks the secret: %s", verb, out)
		}
	}
	if got := holder.Secret.Reveal(); got != value {
		t.Fatalf("Reveal = %q, want the wrapped value", got)
	}
}
