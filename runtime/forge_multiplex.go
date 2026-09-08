package runtime

// One repository, one observation stream.
//
// Every active run asks the forge the same questions about the repository it
// lives in, and with several runs in one repository those questions multiply:
// N runs polling a pull request list, N permission lookups for the same
// reviewer, N rate-limit budgets spent on one answer. Worse, when the forge
// starts refusing, N independent callers each discover that separately and each
// keep asking.
//
// MultiplexedForge is the supervisor's shared reading of that state. It is a
// DECORATOR over the existing adapter rather than a new observation subsystem:
// no new persistence, no message bus, no second normalization. Reads inside one
// short window are answered once and fanned out; writes pass straight through
// and invalidate what they changed; a rate-limit refusal becomes shared
// backoff, so one run learning that the forge wants to be left alone is every
// run learning it.
//
// It is deliberately NOT a cache with a long life. The window is short enough
// that a run never acts on a stale head - the runtime's own staleness rules
// already govern that - and long enough that one supervisor tick over ten runs
// is one set of calls rather than ten.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// DefaultForgeWindow is the coalescing window. It bounds how long one answer
// may serve several runs, and it is short because an observation that is old
// enough to matter is one the runtime should re-derive.
const DefaultForgeWindow = 10 * time.Second

// MultiplexedForge shares one repository's observations across every run.
type MultiplexedForge struct {
	Inner  GitHubAdapter
	Clock  Clock
	Window time.Duration

	mu sync.Mutex
	// answers holds one in-window answer per exact question.
	answers map[string]forgeAnswer
	// backoff is per REPOSITORY, because that is the scope the forge refuses
	// at: a rate limit is about the credential's budget against that
	// repository, not about the run that happened to hit it.
	backoff map[string]forgeBackoff
	// inflight is the request currently in progress for a key, if any. It is
	// what makes coalescing true on a COLD cache: without it, several runs
	// starting together all miss, all call the forge, and the shared
	// observation only ever shares an answer that some earlier tick happened to
	// warm - which is exactly the moment sharing matters least.
	inflight map[string]*forgeCall
	// epoch increments on every invalidation. An in-flight read carries the
	// epoch it started in, and a read that started BEFORE a write is neither
	// joined nor cached afterwards.
	//
	// Without it, coalescing reintroduced the staleness invalidation exists to
	// prevent: a read that began before a publication could still be waited on
	// after it, hand its pre-write answer to the joiner, and then store that
	// answer back into the cache the write had just cleared.
	epoch uint64
	// joins counts callers that joined an in-flight request; see joinCount.
	joins uint64
	// calls counts underlying calls per method, which is what makes "runs
	// share one poll" a testable property rather than a claim.
	calls map[string]int
}

// forgeCall is one in-progress read. Waiters block on done and then read the
// same answer the caller that made the request stored.
type forgeCall struct {
	done  chan struct{}
	value any
	err   error
	// epoch is the invalidation generation this call belongs to.
	epoch uint64
}

type forgeAnswer struct {
	value any
	err   error
	at    time.Time
}

type forgeBackoff struct {
	until time.Time
	err   error
}

// NewMultiplexedForge wraps an adapter.
func NewMultiplexedForge(inner GitHubAdapter, clock Clock) *MultiplexedForge {
	if clock == nil {
		clock = RealClock{}
	}
	return &MultiplexedForge{
		Inner: inner, Clock: clock, Window: DefaultForgeWindow,
		answers: map[string]forgeAnswer{}, backoff: map[string]forgeBackoff{},
		inflight: map[string]*forgeCall{}, calls: map[string]int{},
	}
}

// Calls reports how many underlying calls each method actually made.
// joinCount is how many callers have adopted an in-flight request. It exists
// for tests that must establish that ordering before releasing anything.
func (m *MultiplexedForge) joinCount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.joins
}

func (m *MultiplexedForge) Calls() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	counted := make(map[string]int, len(m.calls))
	for method, count := range m.calls {
		counted[method] = count
	}
	return counted
}

func (m *MultiplexedForge) window() time.Duration {
	if m.Window <= 0 {
		return DefaultForgeWindow
	}
	return m.Window
}

// observe is the one shared read path. Everything it does is stated here so
// each wrapped method stays a one-line binding of a key to a call.
//
// The generic parameter keeps the cached value typed: a cached
// GitHubCheckObservation can never be handed back as a GitHubReviewObservation,
// which a map[string]any without it would make a runtime question.
// callerLocalError reports whether an error describes the CALLER rather than
// the repository. A cancellation or a deadline belongs to whoever made the
// request; it is never another run's answer, whether it would have been reached
// through the cache or by joining an in-flight call.
func callerLocalError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func observe[T any](ctx context.Context, m *MultiplexedForge, repo GitHubRepo, method, key string, call func() (T, error)) (T, error) {
	var zero T
	// joined counts how many times this caller has adopted somebody else's
	// in-flight request. It bounds the retry below to one, so a repository
	// whose owner keeps being cancelled cannot turn a read into a spin.
	joined := 0
retry:
	now := m.Clock.Now()

	m.mu.Lock()
	// A repository the forge asked us to leave alone is not asked again, by
	// ANY run, until the instant it named.
	if wait, ok := m.backoff[repo.String()]; ok {
		if now.Before(wait.until) {
			m.mu.Unlock()
			return zero, wait.err
		}
		delete(m.backoff, repo.String())
	}
	if answer, ok := m.answers[key]; ok && now.Sub(answer.at) < m.window() {
		m.mu.Unlock()
		return typedForgeAnswer[T](key, answer.value, answer.err)
	}
	// A request already in flight for this key is JOINED rather than repeated -
	// but only when it belongs to the CURRENT epoch. One that started before an
	// invalidation would answer with pre-write state, which is precisely what
	// the invalidation was for.
	epoch := m.epoch
	if pending, ok := m.inflight[key]; ok && pending.epoch == epoch {
		// Counted while the lock is still held, so a test can wait for a joiner
		// to have ACTUALLY joined rather than sleeping and hoping. Ordering
		// assertions that rest on a sleep pass when the scheduler is kind and
		// stop testing anything when it is not.
		m.joins++
		m.mu.Unlock()
		// Waiting is cancellable. A joiner blocked on somebody else's slow
		// request would otherwise hold up its own caller - and, through it, a
		// whole supervisor tick - long after its context was done.
		select {
		case <-pending.done:
			// The owner's own cancellation is not this caller's answer. A
			// sibling that merely arrived while a since-stopped run held the
			// request would otherwise be told its live question was cancelled -
			// the same caller-scoped leak the cache refuses, reached by the
			// other path. Fall through and become an owner instead, once.
			if joined == 0 && callerLocalError(pending.err) && ctx.Err() == nil {
				joined++
				goto retry
			}
			return typedForgeAnswer[T](key, pending.value, pending.err)
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	pending := &forgeCall{done: make(chan struct{}), epoch: epoch}
	m.inflight[key] = pending
	m.calls[method]++
	m.mu.Unlock()

	value, err := call()

	m.mu.Lock()
	defer m.mu.Unlock()
	pending.value, pending.err = value, err
	close(pending.done)
	// Only remove the entry if it is still ours: an invalidation may have
	// replaced it, and deleting somebody else's in-flight call would strand
	// its joiners.
	if current, ok := m.inflight[key]; ok && current == pending {
		delete(m.inflight, key)
	}
	// A transient refusal that carries retry timing becomes shared backoff. The
	// forge's own instruction is honoured once for the whole repository instead
	// of being rediscovered by every run.
	//
	// This is recorded BEFORE the epoch check, and deliberately. Cacheability
	// and backoff are facts about different things: an answer describes
	// repository state, which a write can move past, while "stop asking until
	// this instant" describes the forge itself, which no write of ours changes.
	// Discarding the instruction because a sibling wrote something in the
	// meantime would send every other run straight back into the same limit.
	var transient *GitHubTransientError
	if errors.As(err, &transient) {
		if until := retryInstant(now, transient.RateLimit); until.After(now) {
			m.backoff[repo.String()] = forgeBackoff{until: until, err: err}
		}
	}
	// An answer from a superseded epoch is returned to THIS caller - it is the
	// answer its own request produced - and is not cached, because the state it
	// describes is the state a write has already moved past.
	if m.epoch != epoch {
		return typedForgeAnswer[T](key, value, err)
	}
	// A caller's OWN cancellation or deadline is a fact about that caller, not
	// about the repository, so it is returned and never shared. Caching it would
	// let one stopped run answer its siblings' live questions with a
	// cancellation for the rest of the window - a run being stopped would
	// degrade every other run in the same repository, which is the opposite of
	// what sharing an observation stream is for.
	//
	// Other errors ARE cached. A 404 or a 403 describes the repository, which is
	// exactly the kind of observation the siblings should be spared repeating.
	if callerLocalError(err) {
		return value, err
	}
	m.answers[key] = forgeAnswer{value: value, err: err, at: now}
	return value, err
}

// typedForgeAnswer returns a shared answer as the caller's type. A key
// collision across types would be a defect in this file, not a condition of the
// run, so it is surfaced rather than silently re-read.
func typedForgeAnswer[T any](key string, value any, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("multiplexed forge answer for %q is not a %T", key, zero)
	}
	return typed, nil
}

// retryInstant is when the forge said to come back, bounded so a malformed or
// hostile header cannot park a repository indefinitely.
func retryInstant(now time.Time, rate RateLimitObservation) time.Time {
	until := now
	if rate.RetryAfter > 0 {
		until = now.Add(rate.RetryAfter)
	}
	if !rate.ResetAt.IsZero() && rate.ResetAt.After(until) {
		until = rate.ResetAt
	}
	if ceiling := now.Add(watchMaxBackoff); until.After(ceiling) {
		return ceiling
	}
	return until
}

// invalidate drops every cached answer about one repository. A write changed
// the state those answers describe, so serving them afterwards would be
// serving a view the runtime itself has already moved past.
func (m *MultiplexedForge) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = map[string]forgeAnswer{}
	// Reads still in flight belong to the old generation: they may already
	// hold pre-write state. Advancing the epoch is what stops a later caller
	// joining one and stops its owner caching the result.
	//
	// The in-flight map is deliberately NOT cleared as well. It would be a
	// second mechanism for the same invariant, and the redundant one is
	// untestable: with both in place, removing either changes no observable
	// behaviour, so neither can be shown to do anything. The epoch is kept
	// because it states the rule directly - a call from a superseded
	// generation is not joined and not cached - and because clearing the map
	// would also strand the waiters of a call still in progress.
	m.epoch++
}

func forgeKey(repo GitHubRepo, parts ...string) string {
	key := repo.String()
	for _, part := range parts {
		key += "|" + part
	}
	return key
}

// ---------------------------------------------------------------------------
// GitHubAdapter
// ---------------------------------------------------------------------------

var _ GitHubAdapter = (*MultiplexedForge)(nil)

func (m *MultiplexedForge) Issue(ctx context.Context, repo GitHubRepo, number int) (GitHubIssue, error) {
	return observe(ctx, m, repo, "Issue", forgeKey(repo, "issue", itoa(number)), func() (GitHubIssue, error) {
		return m.Inner.Issue(ctx, repo, number)
	})
}

func (m *MultiplexedForge) DiscoverIssues(ctx context.Context, query DiscoveryQuery) (DiscoveryResult, error) {
	return observe(ctx, m, query.Repo, "DiscoverIssues", forgeKey(query.Repo, "discover", query.Label, query.ETag), func() (DiscoveryResult, error) {
		return m.Inner.DiscoverIssues(ctx, query)
	})
}

func (m *MultiplexedForge) FindPullRequests(ctx context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error) {
	return observe(ctx, m, repo, "FindPullRequests", forgeKey(repo, "find", headRef, baseRef), func() ([]GitHubPullRequest, error) {
		return m.Inner.FindPullRequests(ctx, repo, headRef, baseRef)
	})
}

func (m *MultiplexedForge) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	return observe(ctx, m, repo, "PullRequest", forgeKey(repo, "pr", itoa(number)), func() (GitHubPullRequest, error) {
		return m.Inner.PullRequest(ctx, repo, number)
	})
}

func (m *MultiplexedForge) Checks(ctx context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error) {
	return observe(ctx, m, repo, "Checks", forgeKey(repo, "checks", headSHA), func() (GitHubCheckObservation, error) {
		return m.Inner.Checks(ctx, repo, headSHA)
	})
}

func (m *MultiplexedForge) Reviews(ctx context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error) {
	return observe(ctx, m, repo, "Reviews", forgeKey(repo, "reviews", itoa(number), headSHA), func() (GitHubReviewObservation, error) {
		return m.Inner.Reviews(ctx, repo, number, headSHA)
	})
}

func (m *MultiplexedForge) RefSHA(ctx context.Context, repo GitHubRepo, ref string) (RefObservation, error) {
	return observe(ctx, m, repo, "RefSHA", forgeKey(repo, "ref", ref), func() (RefObservation, error) {
		return m.Inner.RefSHA(ctx, repo, ref)
	})
}

// The write path is never coalesced and always invalidates. A publication is a
// side effect, not an observation: performing it once per caller is the point,
// and every cached answer about the repository is about to be wrong.

func (m *MultiplexedForge) CreatePullRequest(ctx context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error) {
	defer m.invalidate()
	return m.Inner.CreatePullRequest(ctx, repo, request)
}

func (m *MultiplexedForge) UpdatePullRequest(ctx context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error) {
	defer m.invalidate()
	return m.Inner.UpdatePullRequest(ctx, repo, number, update)
}

func (m *MultiplexedForge) CommentOnPullRequest(ctx context.Context, repo GitHubRepo, number int, body Publication) error {
	defer m.invalidate()
	return m.Inner.CommentOnPullRequest(ctx, repo, number, body)
}

// ---------------------------------------------------------------------------
// Optional capabilities
// ---------------------------------------------------------------------------
//
// Each is forwarded only when the wrapped adapter actually has it. A decorator
// that implemented these unconditionally would claim a capability the real
// adapter may not have, and feedback admission decides what it does from
// exactly that claim.

func (m *MultiplexedForge) RepositoryPermission(ctx context.Context, repo GitHubRepo, login string) (GitHubPermission, error) {
	inner, ok := m.Inner.(ForgeActorPermissions)
	if !ok {
		return PermissionUnresolved, fmt.Errorf("the configured forge adapter cannot resolve actor permissions")
	}
	return observe(ctx, m, repo, "RepositoryPermission", forgeKey(repo, "permission", login), func() (GitHubPermission, error) {
		return inner.RepositoryPermission(ctx, repo, login)
	})
}

func (m *MultiplexedForge) PullRequestComments(ctx context.Context, repo GitHubRepo, number int) ([]GitHubComment, error) {
	inner, ok := m.Inner.(ForgeConversation)
	if !ok {
		return nil, fmt.Errorf("the configured forge adapter cannot read conversation comments")
	}
	return observe(ctx, m, repo, "PullRequestComments", forgeKey(repo, "pr-comments", itoa(number)), func() ([]GitHubComment, error) {
		return inner.PullRequestComments(ctx, repo, number)
	})
}

func (m *MultiplexedForge) IssueComments(ctx context.Context, repo GitHubRepo, number int) ([]GitHubComment, error) {
	inner, ok := m.Inner.(ForgeConversation)
	if !ok {
		return nil, fmt.Errorf("the configured forge adapter cannot read conversation comments")
	}
	return observe(ctx, m, repo, "IssueComments", forgeKey(repo, "issue-comments", itoa(number)), func() ([]GitHubComment, error) {
		return inner.IssueComments(ctx, repo, number)
	})
}

// Viewer is deliberately NOT coalesced or cached.
//
// Every other read here answers a question about the REPOSITORY, which is why
// answering it once for a window and fanning it out is safe. This one answers a
// question about the CREDENTIAL - who does this token act as - and the
// credential is re-read from its file on every request, so it can change between
// two calls a cache would collapse into one.
//
// The self-loop guard refuses feedback authored by this identity. A stale answer
// therefore means the runtime is comparing against an account it no longer is:
// rotate the token, publish a comment as the new account, and a sibling run
// observing inside the window resolves the OLD identity, fails to recognize the
// comment as its own, and admits it. The window is small; the consequence is the
// loop this branch exists to prevent, so the extra request is the right trade.
//
// The call is still counted, so shared-observation reporting stays truthful
// about what was asked.
func (m *MultiplexedForge) Viewer(ctx context.Context, repo GitHubRepo) (GitHubActor, error) {
	inner, ok := m.Inner.(ForgeViewer)
	if !ok {
		return GitHubActor{}, fmt.Errorf("the configured forge adapter cannot name its own identity")
	}
	return observeUncached(ctx, m, repo, "Viewer", func() (GitHubActor, error) {
		return inner.Viewer(ctx, repo)
	})
}

// observeUncached performs a read that must be FRESH but must still obey the
// repository's shared rate-limit law.
//
// observe() does two separable things: it caches and coalesces answers, and it
// honours and records the forge's own backoff. Only the first is unsafe for a
// credential identity. Skipping both - which is what bypassing observe entirely
// did - meant N active runs could each rediscover the same rate limit and keep
// asking, which is precisely the behaviour the shared observation stream exists
// to prevent.
func observeUncached[T any](ctx context.Context, m *MultiplexedForge, repo GitHubRepo, method string, call func() (T, error)) (T, error) {
	var zero T
	now := m.Clock.Now()

	m.mu.Lock()
	if wait, ok := m.backoff[repo.String()]; ok {
		if now.Before(wait.until) {
			m.mu.Unlock()
			return zero, wait.err
		}
		delete(m.backoff, repo.String())
	}
	m.calls[method]++
	m.mu.Unlock()

	value, err := call()

	// A transient refusal that carries retry timing becomes shared backoff, the
	// same as any other read. The ANSWER is never stored: that is the whole
	// difference between this and observe().
	var transient *GitHubTransientError
	if errors.As(err, &transient) {
		if until := retryInstant(now, transient.RateLimit); until.After(now) {
			m.mu.Lock()
			m.backoff[repo.String()] = forgeBackoff{until: until, err: err}
			m.mu.Unlock()
		}
	}
	return value, err
}

// SupportsFeedbackAdmission reports whether the wrapped adapter can answer the
// questions feedback admission needs. It exists so the composition root can
// decide truthfully whether to advertise the capability at all, rather than
// wrapping and then failing every lookup.
func (m *MultiplexedForge) SupportsFeedbackAdmission() bool {
	// BOTH capabilities are required. An adapter that can resolve a permission
	// but cannot read a comment thread would advertise admission and then fail
	// every conversation read at run time, which reports a broken run where the
	// truth is a configuration that simply cannot carry feedback.
	_, permissions := m.Inner.(ForgeActorPermissions)
	_, conversation := m.Inner.(ForgeConversation)
	// The VIEWER capability is required too. Without it the runtime cannot
	// learn which account it publishes as, and the self-loop guard has nothing
	// to recognize itself by - so it would admit its own comments as ordinary
	// permitted feedback rather than refusing them.
	_, viewer := m.Inner.(ForgeViewer)
	return permissions && conversation && viewer
}

func itoa(n int) string { return strconv.Itoa(n) }
