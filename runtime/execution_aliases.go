package runtime

import "github.com/bogdaniel/zenchron-engineering/execution"

// The execution port vocabulary lives in package execution (ADR-0005, #521).
// These aliases are a TEMPORARY SEAM so the existing implementations and tests
// compile unchanged; they add no meaning of their own. Removal point: #445
// Slice 9, which rewrites the remaining references to the execution names.
type (
	ExecutionProvider        = execution.Port
	ExecutionRequest         = execution.Request
	ExecutionResult          = execution.Result
	ExecutionAttemptRef      = execution.AttemptRef
	InvocationPurpose        = execution.Purpose
	ProviderBudget           = execution.Budget
	ProviderFailure          = execution.Failure
	FailureClass             = execution.FailureClass
	Finding                  = execution.Finding
	UpstreamContext          = execution.UpstreamContext
	UpstreamHandoff          = execution.UpstreamHandoff
	Ref                      = execution.Ref
	Candidate                = execution.Candidate
	Artifact                 = execution.Artifact
	AttemptBound             = execution.AttemptBound
	FeedbackContext          = execution.FeedbackContext
	FeedbackClass            = execution.FeedbackClass
	PriorAttemptObservations = execution.PriorAttemptObservations
	InvocationProvenance     = execution.InvocationProvenance
	TrustMode                = execution.TrustMode
	GitRefusal               = execution.GitRefusal
	GitActorOrigin           = execution.GitActorOrigin
	GitTargetClass           = execution.GitTargetClass
	TerminationOwner         = execution.TerminationOwner
	ProviderNotStartedError  = execution.NotStartedError
	ProviderProgress         = execution.Progress
	ProviderIsolation        = execution.ProviderIsolation
	IsolationLevel           = execution.IsolationLevel
)

const (
	InvocationInitial           = execution.PurposeInitial
	InvocationRemediation       = execution.PurposeRemediation
	InvocationPlanning          = execution.PurposePlanning
	InvocationContinuation      = execution.PurposeContinuation
	InvocationHandoffRepair     = execution.PurposeHandoffRepair
	InvocationIndependentReview = execution.PurposeIndependentReview

	FailureTransientProvider                = execution.FailureTransientProvider
	FailureProviderAccountUnavailable       = execution.FailureProviderAccountUnavailable
	FailureProviderQuota                    = execution.FailureProviderQuota
	FailureControllerShutdown               = execution.FailureControllerShutdown
	FailureProviderRateLimited              = execution.FailureProviderRateLimited
	FailureProviderNoProgress               = execution.FailureProviderNoProgress
	FailureProviderBackgroundWorkUnresolved = execution.FailureProviderBackgroundWorkUnresolved
	FailureProviderUnavailable              = execution.FailureProviderUnavailable
	FailureConnectivity                     = execution.FailureConnectivity
	FailureExecutionIncomplete              = execution.FailureExecutionIncomplete
	FailureRunCancelled                     = execution.FailureRunCancelled
	FailureUnknown                          = execution.FailureUnknown

	OwnerUndecided          = execution.OwnerUndecided
	OwnerProviderExited     = execution.OwnerProviderExited
	OwnerDeadline           = execution.OwnerDeadline
	OwnerInactivity         = execution.OwnerInactivity
	OwnerOperatorStop       = execution.OwnerOperatorStop
	OwnerControllerShutdown = execution.OwnerControllerShutdown
	OwnerNotStarted         = execution.OwnerNotStarted

	IsolationUnproven = execution.IsolationUnproven
	IsolationProven   = execution.IsolationProven
)

// ErrProviderInactive is execution.ErrProviderInactive under its runtime name.
var ErrProviderInactive = execution.ErrProviderInactive
