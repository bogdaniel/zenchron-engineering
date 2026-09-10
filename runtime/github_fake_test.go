package runtime

import (
	"context"
	"sync"
	"testing"
)

// The supervisor genuinely drives several runs at once, and every one of them
// reaches the same forge - the fake included. A test double that races under the
// concurrency the product produces reports a defect in itself, not in the code
// under test, which is exactly what happened the first time a plan drove two
// stage runs in one tick. This holds the double to the concurrency it is
// actually subjected to; removing the mutex fails it under -race.
func TestTheFakeForgeToleratesTheConcurrencyTheSupervisorProduces(t *testing.T) {
	forge := NewFakeGitHubAdapter()
	forge.ViewerActor = GitHubActor{Login: "zenchron-bot", ID: 42, Bot: true}
	forge.Issues[7] = GitHubIssue{Number: 7, Title: "seven", State: "open"}
	repo := GitHubRepo{Owner: "o", Name: "n"}

	var waiting sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for call := 0; call < 40; call++ {
				if _, err := forge.Viewer(context.Background(), repo); err != nil {
					t.Errorf("viewer: %v", err)
					return
				}
				if _, err := forge.Issue(context.Background(), repo, 7); err != nil {
					t.Errorf("issue: %v", err)
					return
				}
				if _, err := forge.RefSHA(context.Background(), repo, "refs/heads/main"); err != nil {
					t.Errorf("ref: %v", err)
					return
				}
			}
		}()
	}
	waiting.Wait()

	if want := 8 * 40 * 3; len(forge.Calls) != want {
		t.Fatalf("recorded %d calls, want %d: a lost append is the same lost write the race warned about", len(forge.Calls), want)
	}
}
