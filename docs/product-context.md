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
graph association resolves through.

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
`work_unit` scope. Reaching `feature`, `product` or `organization` scope
requires `Promote`, which:

- refuses an empty `authorizedBy` - a claim never widens its own scope;
- refuses a target scope that is not strictly above the source's;
- carries the ORIGINAL provenance forward into the durable
  `KnowledgePromotion` audit record, so an organization-scope claim can always
  be traced back to the agent discovery it started as.

There is no code path that lets a directly authored entry land above
`work_unit` scope, and no code path that promotes without an explicit human
authorizer. "Agent discovery never becomes organization truth automatically"
is true by construction, not by convention.

## Graph association (`association.go`)

A `GraphAssociation` links one existing #472 `WorkGraph` id to the `Product`
its units resolve context against. It is an append-only fact, never a
revision: associating a graph changes neither the graph nor the product. The
runtime store enforces one product per graph with a single-column primary key
on `graph_id` - the first association for a graph stands, and a conflicting
second product claiming it is refused.

## Context compilation (`context.go`) - the #480/#479 contract

```go
type Store interface {
    CurrentProduct(productID string) (Product, bool, error)
    CurrentConfiguration(productID string) (ProductConfiguration, bool, error)
    KnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error)
}

type ContextRequest struct {
    ProductID string
    Scope     Scope
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
that can read a sibling product's row. A missing product is an error, never
an empty context, so a caller that named the wrong product id fails closed
instead of silently seeing nothing.

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
