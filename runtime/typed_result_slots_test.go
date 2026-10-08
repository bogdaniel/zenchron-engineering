package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// executeWithHostSlots is what invokeExecution does around a provider: Execute,
// then the host reads the runtime-owned slots.
func executeWithHostSlots(ctx context.Context, provider ExecutionProvider, request ExecutionRequest) (ExecutionResult, typedResults, error) {
	result, err := provider.Execute(ctx, request)
	typed := readTypedResultSlots(request.ReviewerResultPath, request.FeedbackResolutionPath, &result)
	return result, typed, err
}

// writeTypedResultFile is how a test worker states a typed result: the same
// file a real worker writes, at the path the runtime prepared.
func writeTypedResultFile(path string, document any) error {
	if path == "" {
		return errors.New("no typed result slot was granted to this invocation")
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

// A PROVIDER CANNOT SUPPLY A VERDICT OR A RESOLUTION EXCEPT THROUGH THE FILE
// (#521). ExecutionResult carries no field a provider could fill with one, so
// the host-owned slot is the only channel. Re-adding such a field fails here.
func TestExecutionResultCarriesNoTypedResultAProviderCouldInject(t *testing.T) {
	forbidden := map[reflect.Type]bool{
		reflect.TypeFor[*ReviewerResult]():             true,
		reflect.TypeFor[*ReviewerResultRefusedError](): true,
		reflect.TypeFor[*FeedbackResolution]():         true,
		reflect.TypeFor[ReviewerResult]():              true,
		reflect.TypeFor[FeedbackResolution]():          true,
	}
	resultType := reflect.TypeFor[ExecutionResult]()
	for i := range resultType.NumField() {
		if field := resultType.Field(i); forbidden[field.Type] {
			t.Fatalf("ExecutionResult.%s lets a provider state a typed result without the host-owned file", field.Name)
		}
	}
}

// slotFixture is the real CLI adapter on a successful invocation, with both
// slots prepared exactly as invokeExecution prepares them.
func slotFixture(t *testing.T) (CLIAgentProvider, ExecutionRequest, *fakeAgentExecutor) {
	t.Helper()
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	state := t.TempDir()
	reviewPath, err := PrepareReviewerResult(state, request.AttemptRef())
	if err != nil {
		t.Fatal(err)
	}
	resolutionPath, err := PrepareFeedbackResolution(state, request.AttemptRef())
	if err != nil {
		t.Fatal(err)
	}
	request.ReviewerResultPath, request.FeedbackResolutionPath = reviewPath, resolutionPath
	return provider, request, fake
}

// The slot read keeps the adapter's old outcomes exactly: a valid document is
// carried, an absent one is nil with no failure, and an unreadable one fails
// the invocation with the same class and the transcript as its diagnostic.
func TestTheHostSlotReadPreservesTheAdapterOutcomes(t *testing.T) {
	cases := map[string]struct {
		review, resolution string
		wantClass          FailureClass
		wantReview         bool
		wantResolution     bool
	}{
		"absent":             {},
		"valid verdict":      {review: `{"schema_version":"0.1","verdict":"accepted"}`, wantReview: true},
		"invalid verdict":    {review: `{not json`, wantClass: FailureReviewerProtocolIncomplete},
		"valid resolution":   {resolution: validResolutionDocument(t), wantResolution: true},
		"invalid resolution": {resolution: `{not json`, wantClass: FailureVerification},
		// A bad verdict stops before the resolution is read.
		"invalid verdict, valid resolution": {
			review: `{not json`, resolution: validResolutionDocument(t), wantClass: FailureReviewerProtocolIncomplete,
		},
		// A good verdict survives a bad resolution, as it did in the adapter.
		"valid verdict, invalid resolution": {
			review: `{"schema_version":"0.1","verdict":"accepted"}`, resolution: `{not json`,
			wantClass: FailureVerification, wantReview: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			provider, request, _ := slotFixture(t)
			writeRaw(t, request.ReviewerResultPath, tc.review)
			writeRaw(t, request.FeedbackResolutionPath, tc.resolution)

			result, typed, err := executeWithHostSlots(context.Background(), provider, request)
			if err != nil {
				t.Fatal(err)
			}
			if (typed.Review != nil) != tc.wantReview || (typed.Resolution != nil) != tc.wantResolution {
				t.Fatalf("review=%+v resolution=%+v", typed.Review, typed.Resolution)
			}
			if tc.wantClass == "" {
				if result.Outcome != execution.Succeeded || result.Failure != nil || typed.ReviewRefusal != nil {
					t.Fatalf("a readable or absent slot failed the invocation: %+v %+v", result, typed.ReviewRefusal)
				}
				return
			}
			if result.Outcome != execution.Failed || result.Failure == nil || result.Failure.Classification != tc.wantClass {
				t.Fatalf("want a %q failure, got %+v", tc.wantClass, result)
			}
			if result.Failure.RawDiagnosticRef != result.Artifacts[0].Path {
				t.Fatalf("the diagnostic does not name the transcript: %q", result.Failure.RawDiagnosticRef)
			}
			wantRefusal := tc.wantClass == FailureReviewerProtocolIncomplete
			if (typed.ReviewRefusal != nil) != wantRefusal {
				t.Fatalf("review refusal = %+v", typed.ReviewRefusal)
			}
			if wantRefusal && !strings.Contains(typed.ReviewRefusal.Detail, "not a valid") {
				t.Fatalf("the exact decode reason was not retained: %+v", typed.ReviewRefusal)
			}
		})
	}
}

// A failed invocation is never read: neither a verdict nor a resolution it
// left on disk becomes its answer, and an unreadable one cannot change why it
// failed.
func TestTheHostReadsNoSlotOutOfAFailedInvocation(t *testing.T) {
	provider, request, fake := slotFixture(t)
	fake.err = errors.New("exit status 1")
	writeRaw(t, request.ReviewerResultPath, `{"schema_version":"0.1","verdict":"accepted"}`)
	writeRaw(t, request.FeedbackResolutionPath, `{not json`)

	result, typed, _ := executeWithHostSlots(context.Background(), provider, request)
	if typed.Review != nil || typed.Resolution != nil || typed.ReviewRefusal != nil {
		t.Fatalf("a failed invocation contributed a typed result: %+v", typed)
	}
	if result.Failure == nil || result.Failure.Classification == FailureVerification {
		t.Fatalf("the slot read changed the failure: %+v", result.Failure)
	}
}

func writeRaw(t *testing.T, path, document string) {
	t.Helper()
	if document == "" {
		return
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validResolutionDocument(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal(validResolution("review:1"))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
