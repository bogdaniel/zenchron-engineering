// Package product implements the #476 ProductEngineeringEnvironment: the
// durable product-scoped boundary that the existing WorkGraph (#472),
// EngineeringPlan (#64) and Repository Intelligence (#67) owners can be
// associated with and read context from, through narrow contracts.
//
// It is not a second scheduler, run database, event journal, policy system or
// authority system. A Product stores REFERENCES to the authoritative owners of
// repositories, policy, project models, graphs and runs; it never duplicates
// their bodies. Execution still happens entirely through the existing runtime.
//
// This package is pure domain logic: identity, revisioning, validation and
// context projection. Persistence lives in runtime's SQLite store, behind the
// narrow Store interface this package defines in context.go.
package product
