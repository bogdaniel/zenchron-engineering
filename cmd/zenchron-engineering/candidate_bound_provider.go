package main

// candidateBoundProvider, kept apart from autonomy.go - which sits at its
// frozen file-size ceiling - as its own cohesive unit: binding the tool
// broker to one run's candidate workspace and owning operation.

import (
	"context"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// candidateBoundProvider binds the tool broker to the candidate workspace named
// by each request. OpenAIProvider refuses a broker bound to any other tree, and
// the tree only exists once a run has been created, so the binding cannot be
// made when the provider is constructed.
type candidateBoundProvider struct{ base runtime.OpenAIProvider }

// The wrapper must keep every capability of the provider it binds (#522).
var (
	_ runtime.IsolationReporter = candidateBoundProvider{}
	_ runtime.ToolchainProber   = candidateBoundProvider{}
)

func (p candidateBoundProvider) Isolation() runtime.ProviderIsolation { return p.base.Isolation() }

// MissingTools forwards the container probe (#522). Without it the runtime's
// capability check misses and probes required tools on the host PATH instead
// of in the sandbox the OpenAI loop runs commands in.
func (p candidateBoundProvider) MissingTools(ctx context.Context, required []string) []string {
	return p.base.MissingTools(ctx, required)
}

// Execute binds the two things the broker cannot supply itself: WHICH workspace
// this invocation may touch, and WHICH runtime operation owns the Docker
// lifecycle of anything it brokers.
//
// Both are refused when absent rather than defaulted. A brokered command with
// no owning operation has no durable record a crashed controller could
// reconcile against, and the alternatives - a fixed global id, the process id,
// a random or model-supplied string - would each let recovery target a
// container this operation does not own.
func (p candidateBoundProvider) Execute(ctx context.Context, request runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	bound, err := p.bind(request)
	if err != nil {
		return runtime.ExecutionResult{}, err
	}
	return bound.Execute(ctx, request)
}

// bind is the binding itself, separated so a test can assert what the provider
// would have been given without contacting a model or a daemon.
func (p candidateBoundProvider) bind(request runtime.ExecutionRequest) (runtime.OpenAIProvider, error) {
	if strings.TrimSpace(request.CandidateDir) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires the runtime-owned candidate workspace")
	}
	if strings.TrimSpace(request.OperationID) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires the runtime operation that authorized it; without it a brokered container has no exact identity to reconcile")
	}
	if strings.TrimSpace(p.base.Broker.Sandbox.StateDir) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires a runtime-owned state directory for its Docker operation record")
	}
	bound := p.base
	bound.Broker.CandidateDir = request.CandidateDir
	bound.Broker.Sandbox.OperationID = request.OperationID
	return bound, nil
}
