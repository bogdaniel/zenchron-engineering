package runtime

// Referenced same-repository issue context, hydrated through the governed forge
// boundary before a reasoning invocation.
//
// The #119 dogfood planner reported that "individual issue bodies were
// unavailable locally, so this proposal uses the pinned cohort description and
// repository evidence". #119 is deliberately a meta-issue over five others, so
// the planner was reasoning about a cohort it had never read. These tests fix
// who reads those issues (the controller), when (once, before the invocation),
// with what standing (untrusted), and what happens when one cannot be read (it
// becomes visible state, not silence).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The citation set is extracted from the PINNED text and is deterministic.
func TestReferencedIssueNumbersAreExplicitCitationsOnly(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
		want  []int
	}{
		{name: "ordinary cohort", body: "resolve #110, #111, #112, #113 and #118", want: []int{110, 111, 112, 113, 118}},
		{name: "from the title too", title: "follow-up to #7", body: "see #9", want: []int{7, 9}},
		{name: "self references are dropped", body: "this issue #119 tracks #110", want: []int{110}},
		{name: "duplicates collapse", body: "#110 and again #110", want: []int{110}},
		{name: "ascending regardless of order", body: "#118 then #110", want: []int{110, 118}},
		{name: "a fragment in a path is not a citation", body: "see docs/x#4 and /issues/#5", want: nil},
		{name: "a colour is not a citation", body: "use #ff0000", want: nil},
		{name: "an anchor in a word is not a citation", body: "sha256#4", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := referencedIssueNumbers(119, tc.title, tc.body)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("referenced issues = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every referenced issue is read once through the forge, pinned by digest, and
// carried as untrusted text. The provider is never asked to fetch anything.
func TestReferencedIssuesArePinnedThroughTheForgeBoundary(t *testing.T) {
	fixture := newPhase8Fixture(t)
	primary := fixture.forge.Issues[fixture.issue]
	primary.Body = UntrustedText(fmt.Sprintf("resolve #%d and #%d", fixture.issue+1, fixture.issue+2))
	fixture.forge.Issues[fixture.issue] = primary
	for _, number := range []int{fixture.issue + 1, fixture.issue + 2} {
		fixture.forge.Issues[number] = GitHubIssue{
			Number: number, URL: fmt.Sprintf("https://github.com/acme/repo/issues/%d", number),
			Title: UntrustedText(fmt.Sprintf("cohort member %d", number)),
			Body:  UntrustedText(fmt.Sprintf("the invariant for %d", number)),
			State: GitHubOpen, UpdatedAt: time.Unix(1_700_000_100, 0).UTC(),
			Author: GitHubActor{Login: "operator", ID: 7},
		}
	}

	intent, err := fixture.runtime.CompilePlanIntent(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.References) != 2 {
		t.Fatalf("references = %#v", intent.References)
	}
	for i, reference := range intent.References {
		switch {
		case !reference.Available:
			t.Fatalf("reference %d is unavailable: %s", i, reference.Detail)
		case reference.Digest == "":
			t.Fatalf("reference %d is not pinned by digest: %#v", i, reference)
		case reference.Repository != "acme/repo":
			t.Fatalf("reference %d names repository %q", i, reference.Repository)
		case !strings.Contains(reference.Body, "the invariant for"):
			t.Fatalf("reference %d carries no engineering text: %#v", i, reference)
		case reference.SnapshotPath == "":
			t.Fatalf("reference %d has no local-only pinned snapshot", i)
		}
	}
	// The referenced text is NOT folded into the objective, which is what the
	// plan document carries and what its digest is over.
	for _, reference := range intent.References {
		if strings.Contains(intent.Objective, reference.Body) {
			t.Fatal("referenced issue text leaked into the plan objective, which the plan digest is over")
		}
	}
	// The durable projection carries identity and provenance and no third-party
	// text, exactly like every other journal payload.
	payloads := intent.ReferencePayloads()
	if len(payloads) != 2 || !payloads[0].Available || payloads[0].Digest == "" {
		t.Fatalf("reference payloads = %#v", payloads)
	}
}

// A referenced issue that cannot be read becomes visible product state. It does
// not stop planning, and it does not silently become an impoverished plan.
func TestAnUnreadableReferencedIssueBecomesVisibleState(t *testing.T) {
	fixture := newPhase8Fixture(t)
	primary := fixture.forge.Issues[fixture.issue]
	primary.Body = UntrustedText(fmt.Sprintf("resolve #%d", fixture.issue+9))
	fixture.forge.Issues[fixture.issue] = primary

	intent, err := fixture.runtime.CompilePlanIntent(context.Background(), fixture.issue)
	if err != nil {
		t.Fatalf("one unreadable reference stopped planning entirely: %v", err)
	}
	if len(intent.References) != 1 {
		t.Fatalf("references = %#v", intent.References)
	}
	reference := intent.References[0]
	if reference.Available {
		t.Fatal("an issue the forge does not have was reported as available")
	}
	if reference.Detail == "" {
		t.Fatal("an unavailable reference records no reason")
	}
	if payloads := intent.ReferencePayloads(); len(payloads) != 1 || payloads[0].Available || payloads[0].Detail == "" {
		t.Fatalf("the unavailable reference is not durable state: %#v", payloads)
	}
}

// The fan-out is BOUNDED, and the bound is stated rather than applied silently.
func TestReferenceHydrationFanOutIsBoundedAndStated(t *testing.T) {
	fixture := newPhase8Fixture(t)
	var citations []string
	for i := 1; i <= maxHydratedReferences+3; i++ {
		number := fixture.issue + i
		citations = append(citations, fmt.Sprintf("#%d", number))
		fixture.forge.Issues[number] = GitHubIssue{
			Number: number, URL: fmt.Sprintf("https://github.com/acme/repo/issues/%d", number),
			Title: "member", Body: "body", State: GitHubOpen,
			UpdatedAt: time.Unix(1_700_000_100, 0).UTC(), Author: GitHubActor{Login: "operator", ID: 7},
		}
	}
	primary := fixture.forge.Issues[fixture.issue]
	primary.Body = UntrustedText("resolve " + strings.Join(citations, ", "))
	fixture.forge.Issues[fixture.issue] = primary

	intent, err := fixture.runtime.CompilePlanIntent(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	hydrated, stated := 0, false
	for _, reference := range intent.References {
		if reference.Available {
			hydrated++
		}
		if reference.Issue == 0 && strings.Contains(reference.Detail, "further referenced issues were not hydrated") {
			stated = true
		}
	}
	if hydrated > maxHydratedReferences {
		t.Fatalf("hydrated %d references, above the %d bound", hydrated, maxHydratedReferences)
	}
	if !stated {
		t.Fatalf("the fan-out bound was applied silently: %#v", intent.References)
	}
}

// Hydration follows explicit citations from the PRIMARY issue only. A cited
// issue that cites others does not drag them in: this is a bounded read, not a
// crawler.
func TestReferenceHydrationDoesNotRecurse(t *testing.T) {
	fixture := newPhase8Fixture(t)
	first, second := fixture.issue+1, fixture.issue+2
	primary := fixture.forge.Issues[fixture.issue]
	primary.Body = UntrustedText(fmt.Sprintf("resolve #%d", first))
	fixture.forge.Issues[fixture.issue] = primary
	fixture.forge.Issues[first] = GitHubIssue{
		Number: first, URL: "https://github.com/acme/repo/issues/1",
		Title: "first", Body: UntrustedText(fmt.Sprintf("depends on #%d", second)),
		State: GitHubOpen, UpdatedAt: time.Unix(1_700_000_100, 0).UTC(),
		Author: GitHubActor{Login: "operator", ID: 7},
	}
	fixture.forge.Issues[second] = GitHubIssue{
		Number: second, URL: "https://github.com/acme/repo/issues/2",
		Title: "second", Body: "leaf", State: GitHubOpen,
		UpdatedAt: time.Unix(1_700_000_100, 0).UTC(), Author: GitHubActor{Login: "operator", ID: 7},
	}

	intent, err := fixture.runtime.CompilePlanIntent(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.References) != 1 || intent.References[0].Issue != first {
		t.Fatalf("hydration followed more than the primary issue's own citations: %#v", intent.References)
	}
}

// A plan that COMPILED still records what its planner was given. The gap
// matters most when the plan looks fine: an operator approving a plan reasoned
// from four of five referenced issues should be able to see that before they
// approve it, not discover it from the plan's own prose.
func TestHydrationProvenanceSurvivesOnASuccessfulProposal(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	input.Reasoned[0].Independence = nil
	input.Reasoned[1].Independence = nil

	plan, err := f.service.Propose(context.Background(), input)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	snapshot, err := f.store.ReplayPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.References) != 2 {
		t.Fatalf("the hydration provenance was lost on a successful proposal: %#v", snapshot.References)
	}
	if snapshot.References[1].Available || snapshot.References[1].Detail == "" {
		t.Fatalf("the unavailable reference did not survive: %#v", snapshot.References[1])
	}
	// And it survives a restart, because it is a journal projection.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ReplayPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.References) != len(snapshot.References) {
		t.Fatalf("hydration provenance changed across a restart: %#v", after.References)
	}
}
