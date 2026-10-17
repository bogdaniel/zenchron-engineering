# ProductEngineeringEnvironment

The `product` package (#476) is the durable product-level boundary that
existing owners - the WorkGraph (#472), EngineeringPlan (#64) and, later,
Repository Intelligence (#67) - associate against and read context from. It is
a new library contract, not a second runtime: there is no CLI surface, no
scheduler, no run database and no policy or authority system here. Everything
in this package is pure domain logic plus one narrow SQLite-backed persistence
layer (`runtime/product_store.go`), reusing the exact database, canonical JSON
and migration conventions #472 and #64 already established.

## The product law

Each product has its own durable engineering world. Product A's history,
configuration and knowledge never leak into Product B's, even when both
products happen to share a repository. Knowledge is shared beyond its origin
only through an explicit, authorized, provenance-preserving promotion - never
automatically because an agent discovered it.

## Why this is not a ninth collapsed distinction

AGENTS.md already names eight distinctions this product's surface frequently
collapses. #476 adds a ninth, stated here rather than left to be rediscovered:

**`product.ProductContext` != #64 `ContextPolicy`.** A `ContextPolicy` selects
which context CLASSES an assignment receives (#64's planning package).
`product.ProductContext` is WHICH PRODUCT's repositories, configuration and
knowledge a request resolves against. The two compose - a `ContextPolicy`
might request "product knowledge" as a class, and `CompileContext` is what
answers it - but neither is the other's storage, and a `ContextPolicy` grants
no authority to see another product's state.

## Identity and structural revisioning (`product.go`)

```text
Product
  ID            derived from Name (ProductID), like a WorkGraph's identity
  Name          operator's stable name
  Revision      content-addressed, immutable once adopted
  Repositories  append-only across revisions - never removed or rebound
```

`ProductProposal.Compose` builds a candidate revision; `Validate` is the
complete deterministic admission check; `ValidateProductMutation` is the gate
a proposed next revision passes - it refuses removing or rebinding a
repository an earlier revision already named, which is exactly the kind of
change that would retroactively alter which product's context an existing
graph association resolves through. The runtime store enforces this at the
write boundary itself, inside one `BEGIN IMMEDIATE` transaction
(`runtime/product_store.go`): the first revision must be 1, a later one must
be exactly the successor of whatever is current, and `ValidateProductMutation`
runs against that exact current revision before anything is written - a
caller cannot skip the check by calling `AdoptProductRevision` directly, and
two concurrent writers proposing disagreeing "next" revisions can never both
succeed. The same pattern, plus a precondition that the product already
exists, applies to `ProductConfiguration` revisions below.

## Mutable configuration, frozen history (`configuration.go`)

`ProductConfiguration` is the MUTABLE half: an operator may adopt a new
revision at any time. "Mutable" describes the product's current pointer, not
any one revision - each `ProductConfiguration` revision is itself immutable
and content-addressed, exactly like a `Product` or `WorkGraph` revision. A unit
that was admitted against revision N keeps revision N forever. The freeze is
**not** in `domain.ContractProvenance` - that records the project model and
policy a contract compiled from, and has never carried a product-configuration
revision. It is in the `orchestration.Batch` the unit's child run was created
from (`Batch.Product`, a `product.ContextPin`; see "Execution integration"
below). Adopting revision N+1 here never rewrites what N said, and never reaches
a unit already admitted.

## The knowledge scope ladder (`scope.go`, `knowledge.go`)

```text
execution -> work_unit -> feature -> product -> organization
```

A claim may be authored directly (`NewKnowledgeEntry`) only at `execution` or
`work_unit` scope, and only bound to the exact `OwnerRef` (the execution or
work unit) that produced it. Reaching `feature`, `product` or `organization`
scope requires `Promote`, which:

- refuses an empty `authorizedBy` - a claim never widens its own scope;
- refuses a target scope that is not strictly above the source's;
- carries the ORIGINAL provenance forward into the durable
  `KnowledgePromotion` audit record, so an organization-scope claim can always
  be traced back to the agent discovery it started as.

**Visibility is never "scope rank at or below the request".** That would let a
product-scoped reader see every unpromoted agent discovery in the product -
exactly the leak #476's acceptance refuses - and, just as importantly, a scope
rank alone is not an AUDIENCE: promoting an execution claim to `work_unit`
scope has to name WHICH work unit it is for, or every work unit in the
product could read it. Three separate reads exist:

- `Store.OwnedKnowledge` - a raw discovery, visible to its own `OwnerRef`
  alone, at any scope;
- `Store.WidelyPromotedKnowledgeAtOrBelow` - a promotion to `product` or
  `organization` scope, which carries no narrower audience and is visible to
  anyone reading at or below that scope;
- `Store.AudiencedPromotedKnowledge` - a promotion to `work_unit` or
  `feature` scope, visible ONLY to the exact `AudienceRef` it named.

`RecordKnowledgeEntry` refuses a promotion-provenance entry outright, so the
only way an entry gets an audit record is `RecordPromotion`, which:

- re-derives the claimed source from storage - its product, scope, provenance
  and statement - rather than trusting the caller's in-memory documents;
- cross-checks the promoted entry against its own audit record via
  `product.ValidatePromotionConsistency` - not merely that each independently
  validates, which two mutually contradictory documents can both do: the
  promoted entry's scope, audience, producer and creation time must each
  agree with what the record claims;
- refuses a conflicting replay - the same promotion id with a stored audit
  document that disagrees with the one being written - transactionally,
  rather than silently ignoring it the way a blind `ON CONFLICT DO NOTHING`
  would (which could otherwise commit a new, unaudited promoted entry while
  dropping the audit row underneath it).

`Validate` also recomputes and checks every entry's and every promotion
record's own content id (which, for a `KnowledgePromotion`, now covers the
promoted entry's id too), so neither can be hand-crafted with an id that
disagrees with its fields, and two different promoted entries can never share
one promotion id.

There is no code path that lets a directly authored entry land above
`work_unit` scope, and no code path that promotes without a non-empty
`authorizedBy`. **What this does NOT yet mean**: `authorizedBy` is a
caller-supplied identity string, exactly like every other `RequestedBy` field
in this codebase (`Product.RequestedBy`, `WorkGraph.RequestedBy`) - it is not
cryptographically verified against an authenticated session, because #476
introduces no second authority or identity system, and no existing one in
this repository fits a knowledge-promotion decision without being misused.
Today's actual caller is always first-party Go code in this process; real
authentication of "who" is deferred until a human-facing caller (#479, or an
operator CLI) establishes identity the way `github.credential_mode` already
does for the review loop, and that caller is expected to pass a verified
identity through, not invent a new one here.

## Graph association (`association.go`)

A `GraphAssociation` links one existing #472 `WorkGraph` id to the `Product`
its units resolve context against. It is an append-only fact, never a
revision: associating a graph changes neither the graph nor the product. The
runtime store enforces one product per graph with a single-column primary key
on `graph_id` - the first association for a graph stands, and a conflicting
second product claiming it is refused. It also verifies BOTH sides exist,
inside the same transaction as the insert: a graph id that was never adopted,
or a product id that was never adopted, is refused rather than stored as a
disconnected identity nothing else can ever resolve.

## Context compilation (`context.go`) - the #480/#479 contract

```go
type Store interface {
    CurrentProduct(productID string) (Product, bool, error)
    CurrentConfiguration(productID string) (ProductConfiguration, bool, error)
    ConfigurationRevision(productID string, revision int) (ProductConfiguration, bool, error)
    OwnedKnowledge(productID string, scope Scope, ownerRef string) ([]KnowledgeEntry, error)
    WidelyPromotedKnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error)
    AudiencedPromotedKnowledge(productID string, scope Scope, audienceRef string) ([]KnowledgeEntry, error)
}

type ContextRequest struct {
    ProductID string
    Scope     Scope
    // OwnerRef is required at ScopeExecution/ScopeWorkUnit, so a caller can
    // only ever see knowledge it authored itself. Whenever non-empty, it also
    // doubles as the audience key for a work_unit-scope promotion.
    OwnerRef string
    // FeatureRef, when known, is the audience key for a feature-scope
    // promotion. Always optional: omitting it means no feature-targeted
    // promotions, the safe default, not an error.
    FeatureRef string
    // ConfigurationRevision pins the exact revision a bound caller already
    // froze into its own contract. Nil means "current".
    ConfigurationRevision *int
}

type ProductContext struct {
    ProductID     string
    Scope         Scope
    Repositories  []string
    Configuration *ProductConfiguration
    Knowledge     []KnowledgeEntry
}

func CompileContext(store Store, request ContextRequest) (ProductContext, error)
```

`runtime.SQLiteOperationStore` satisfies `product.Store` directly (asserted at
compile time in `product_store.go`). The runtime's own caller is
`runtime/product_context.go`, described under "Execution integration"; nothing
else in the runtime compiles a unit's product context.

A `ConfigurationRevision` pin of `0` means "the product had no configuration
when the caller was frozen" and yields none, so a configuration adopted later
still cannot reach it. Revisions start at 1, so `0` is otherwise meaningless.

Every `Store` method is scoped by the exact `productID` `CompileContext` was
given, and it is passed to every one of them - there is no step in this file
that can read a sibling product's row. A missing product, or a missing
PINNED configuration revision, is an error, never an empty or substituted
context: a caller that named the wrong product id, or whose frozen revision
has somehow disappeared, fails closed instead of silently seeing nothing or
silently seeing a newer configuration than the one it was bound to.
`MaxContextKnowledgeEntries` bounds each of the owned and promoted reads, so
a product with many entries cannot hand one caller an unbounded context.

## Execution integration (#480)

An admitted WorkUnit of a product-associated WorkGraph executes against the
product context it was admitted with. This is the only place the runtime
compiles product context for a worker, and it adds no scheduler, run store,
policy system or instruction channel.

```text
product_graph_associations (graph_id -> product_id)   runtime-owned, insert-only
        |
        |  ADMISSION: the instant a unit's orchestration.Batch is first written
        |  (activateWorkUnit, startObjectiveStage, a remediation epoch)
        v
pinProductContext -> product.CompileContext -> product.NewContextPin
        v
Batch.Product (product.ContextPin)     first write wins, NOT part of the batch id
        |
        |  EXECUTION: EngineeringRuntime.workUnit, before the provider is reached
        v
unitProductContext -> ContextPin.Resolve -> product.CompileContext (pinned revision)
        v
execution.Request.Product -> providerPrompt / productBlock -> the worker
```

**Where each identity comes from.**

| Identity | Authoritative producer | Consumer |
| --- | --- | --- |
| product | `product_graph_associations`, read by the batch's `Origin.GraphID` | `pinProductContext`, `unitProductContext` |
| owner / audience | `product.WorkUnitOwnerRef(graphID, unitID)` from the batch's `Origin` | `unitContextRequest` |
| configuration revision | the `ContextPin` written at admission | `ContextPin.Resolve` |
| knowledge set | `ContextPin.KnowledgeIDs` written at admission | `ContextPin.Resolve` |
| context class entitlement | the frozen assignment's `Context.Included`, else `planning.RoleContextClasses` | `receivesProductContext` |

No product id, owner, feature or revision is read from model output, a provider
argument or a request. `FeatureRef` is left empty because the runtime has no
authoritative feature identity yet; an empty one is the safe default and means
no feature-targeted promotions are delivered.

**What is frozen, and why it is not in the batch identity.** The pin records the
configuration revision and content digest, the ids of the knowledge entries
(each content-addressed) the unit was admitted with, and a digest of both.
`Batch.Product` is deliberately outside the batch's identity digest: the
identity is what a crashed activation replays onto, and a newer configuration
must not name a second batch and a second run for one unit. The first write
wins and every replay reads it back, so retries, restarts, generation changes
and repeated admission all see one context. Knowledge only grows, so a claim
promoted after admission is excluded by omission from the pin. A remediation
epoch inherits its unit's first pin rather than compiling a new one.

**Scope.** A unit reads at `ScopeProduct` with its own `WorkUnitOwnerRef`: its
product's configuration, widely promoted knowledge, and promotions audienced to
exactly that unit. `CompileContext` still owns every visibility rule. A unit's
own *unpromoted* discoveries are not delivered at this scope (request scopes are
alternatives, not additive), so they re-enter only by promotion. Nothing in the
runtime records raw discoveries yet.

**Delivery.** `execution.Request.Product` is a plain data record
(`execution.ProductContext`) with no field for an `EngineeringPolicy` or
`ContextPolicy` reference, a tool, a permission, a budget or a trust level, so a
configuration structurally cannot widen or replace the work contract. It is
rendered by `providerPrompt` as an `UNTRUSTED-PRODUCT-CONTEXT` frame, the same
framed-data mechanism upstream outputs use, with every value neutralized so none
can close the frame.

**ContextPolicy.** The closed `ContextClass` vocabulary has no product class.
Product context travels as `project_model_facts`: a unit with a frozen
assignment receives it only if that assignment's `Context.Included` has the
class, and a unit with none is judged by `planning.RoleContextClasses`, the one
role table. A `ContextPolicy` can therefore exclude product context and can
never exclude the required governance classes.

**Failure semantics.** Before any model invocation, `workUnit` refuses when: the
graph is associated and the batch has no pin; the graph has no association but
the batch is pinned; the pin is for another product; the graph's repository is
not one of the product's; the run's plan binding differs from the graph's; the
pinned configuration revision is missing or its content changed; a pinned
knowledge entry is no longer readable by the unit; or the pin does not match its
own digest. It never falls back to another product, a current configuration or
an empty context. An unassociated graph is unchanged: no pin, no block.

**Association ordering.** `AssociateWorkGraph` refuses a graph that already has
a stored unit batch, because that unit froze no context. Replaying the same
association stays idempotent. Objective intake associates before any plan stage
starts, so it is unaffected.

**Failure windows.** A crash after the pin is computed and before the batch is
written leaves nothing; the retry computes a fresh pin. A crash after the batch
and before its activation row replays onto the stored batch and keeps its pin.
The batch write and the association are each checked against the other inside
their own SQLite transaction (`requireBatchProductBinding`; the association
counts stored batches, activation row or not), so an associated graph can never
hold an unpinned admitted unit and a pin can never name a product the graph is
not associated with; execution re-checks the same facts.
Rolling the binary back leaves pinned batches unreadable to the old binary,
whose strict decoder rejects the unknown `product` member.

**Plan-stage reviewer runs.** An objective plan's reviewer (non-producing) agent
stage is a plan-stage run with no work-graph batch, so its pin rides on the run's
own `RunPlanBinding.Product` (`GraphID` + `ContextPin`). `startObjectiveStage`
writes it at run creation (`planStageProduct`); a replay adopts the stored run,
so the first write stands, and `workUnit` resolves it through the same
`unitProductContext` checks, with the stage id as its owner so it holds no
producer's audience. Gate stages create no run and need none. A stage run
created before its graph was associated is not pinned and runs unassociated;
objective intake associates before approval, so none exists in practice. The
reconciler wiring is proved by `TestTheReconcilerCreatesAReviewerStageRunWithTheProductPin`,
which runs the real plan reconciler and supervisor until the reviewer stage run
exists and reads the pin off its durable plan binding; the resolution seam is
`TestAPlanStageRunWithNoBatchKeepsTheProductContextItWasCreatedWith`.

**Not covered here.** Production adoption of the Agent Execution Kernel is
#518's, gated on #445; this integration reaches the worker through the existing
host-owned `execution.Request` and changes no provider. The kernel adapter
classifies `Request.Product` as refused, so a unit admitted with product context
fails closed there instead of running without it.

## What #476 deliberately does not build

- **No CLI.** Nothing here is operator-facing yet; #479 (Experience Plane) is
  the first consumer with a human-facing surface, and it reads this package's
  Go contract rather than shelling out to a new command.
- **No Repository Intelligence.** Product.Repositories are bare repository
  identifiers; #476 stores no analysis, dependency graph or project model of
  its own. #67 remains that owner, consumed by reference, not yet wired in.
- **No release/incident history, design/product artifact references, or
  team/role/authority references.** The issue's "minimum model" lists these as
  conceptually in scope; this batch builds identity, configuration, graph
  association and the knowledge ladder - the primitives #480 and #479
  immediately need - and defers the remaining reference kinds rather than
  stub them out unused. Adding one is a new, narrow reference field plus its
  own store column, not a redesign: nothing here forecloses it.
- **No automatic knowledge demotion or revocation.** A promoted claim can be
  superseded by authoring a new one; nothing here retracts a wrong claim
  already promoted. That is real, deferred work for whoever first needs it.
- **No UI for reviewing or disputing a promotion.** The audit trail
  (`KnowledgePromotion`) is durable and complete; presenting it to a human is
  #479's job.
- **No cross-repository or cross-product policy.** A product's configuration
  is its own; #476 does not compile or enforce `EngineeringPolicy` - it only
  carries a reference to one, exactly as #64's `ContractProvenance` already
  does for a run.
- **No cross-product organization knowledge sharing.** `PromotedKnowledgeAtOrBelow`
  is still scoped by one `productID`, so an entry explicitly promoted all the
  way to `ScopeOrganization` is today visible only within the product that
  promoted it, not reusable by a second product. The scope ladder's top step
  is real (it survives Promote's direction check and is distinguishable from
  `ScopeProduct`), but true cross-product sharing needs a separate,
  explicitly governed read path with no product filter, which this batch
  does not add. This is a reduction in #476's full scope, stated here rather
  than left to be rediscovered: #476 is not closed by this PR alone.
- **No cryptographically verified `authorizedBy` identity.** See the scope
  ladder section above - it is a caller-asserted string today, consistent
  with every other `RequestedBy` field in this codebase, not yet backed by an
  authenticated session. Closing this needs a real caller establishing
  identity, not new machinery here.
- **`OwnerRef`, `ProductID` and `ConfigurationRevision` are selectors, not
  proofs of authority.** `CompileContext` trusts its caller to say which
  product, owner and configuration revision it is. The runtime's caller
  (`runtime/product_context.go`, see "Execution integration") derives them from
  the graph association, the batch's own graph and unit ids, and the pin written
  at admission, never from caller input. Any other caller of this package's
  direct interface is trusted first-party Go code, not an untrusted-input
  boundary.
