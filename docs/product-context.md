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
and content-addressed, exactly like a `Product` or `WorkGraph` revision. A run
that already resolved `Policy` or `ContextPolicy` from revision N keeps
referencing revision N's exact values forever (via its own
`domain.ContractProvenance`, which this package never touches); adopting
revision N+1 here never rewrites what N said.

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
compile time in `product_store.go`), so a caller already holding the
runtime's store - the #480 WorkGraph compiler, or the #479 Experience Plane -
calls `product.CompileContext(store, request)` with no adapter in between.

Every `Store` method is scoped by the exact `productID` `CompileContext` was
given, and it is passed to every one of them - there is no step in this file
that can read a sibling product's row. A missing product, or a missing
PINNED configuration revision, is an error, never an empty or substituted
context: a caller that named the wrong product id, or whose frozen revision
has somehow disappeared, fails closed instead of silently seeing nothing or
silently seeing a newer configuration than the one it was bound to.
`MaxContextKnowledgeEntries` bounds each of the owned and promoted reads, so
a product with many entries cannot hand one caller an unbounded context.

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
  proofs of authority.** `CompileContext` trusts the caller to say which
  product, owner and configuration revision it is; it does not verify that
  the caller is actually entitled to read as that owner or that product, or
  that an omitted `ConfigurationRevision` wasn't supposed to be pinned. The
  eventual #480/#479 runtime adapter must derive these from the runtime's own
  admitted facts - the graph association a WorkUnit's invocation already
  belongs to, and the exact revision its own `domain.ContractProvenance`
  recorded - rather than accept them as freely chosen caller input. Until
  that adapter exists, this package's direct interface is for trusted,
  first-party Go code only, not an untrusted-input boundary.
