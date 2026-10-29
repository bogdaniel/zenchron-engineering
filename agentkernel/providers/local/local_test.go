package local

import (
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{
		Wire: true,
		Build: func(t *testing.T, env conformance.Env) api.Provider {
			p, err := New(Config{Endpoint: "http://127.0.0.1:1/complete", Doer: env.Transport,
				Credentials: env.Credentials, MaxResponseBytes: env.MaxResponseBytes})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		Encode: func(r api.ProviderResponse) []byte {
			data, _ := json.Marshal(r)
			return data
		},
		Inspect: conformance.InspectNeutral,
	})
}

func TestUnknownFieldIsMalformed(t *testing.T) {
	if _, perr := parse([]byte(`{"stop":"end","usage":{"input":null,"output":null,"cached_input":null},"extra":1}`)); perr == nil {
		t.Fatal("unknown field accepted")
	}
}
