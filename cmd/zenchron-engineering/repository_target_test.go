package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestRepositoryTargetPropagatesCheckoutGuidance(t *testing.T) {
	dir := t.TempDir()
	_, err := repositoryTarget(dir, "")
	var refusal *runtime.RepositoryTargetError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "cd into") {
		t.Fatalf("shared checkout guidance was lost: %v", err)
	}
}
