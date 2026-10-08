package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestHostPortDocumentTask(t *testing.T) {
	var log bytes.Buffer
	res, err := run(context.Background(), t.TempDir(), &log)
	if err != nil {
		t.Fatal(err)
	}
	if res.Termination.Outcome != api.OutcomeCompleted || !strings.Contains(res.FinalText, "60 days") {
		t.Fatalf("termination %+v, final %q", res.Termination, res.FinalText)
	}
	refused := 0
	for _, o := range res.Observations {
		if o.Kind == api.EventToolRefused {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("want exactly the ungranted document refused, got %d refusals", refused)
	}
	if lines := strings.Count(log.String(), "\n"); int64(lines) != res.EventCount {
		t.Fatalf("host sink holds %d events, result counts %d", lines, res.EventCount)
	}
	if strings.Contains(log.String(), "host-secret") {
		t.Fatal("credential value reached the event log")
	}
}
