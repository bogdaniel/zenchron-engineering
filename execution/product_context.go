package execution

// ProductContext is the product context a WorkGraph unit's invocation receives
// (#480). It is a plain data record the host fills from the context it froze
// at admission; this package never reads a store or decides visibility.
//
// What it CANNOT carry is as deliberate as what it can: there is no field for
// an EngineeringPolicy or ContextPolicy reference, a tool, a permission, a
// budget or a trust level, so a product configuration has no path to widen or
// replace the work contract the unit was approved under. It is information the
// worker may use, never authority.
type ProductContext struct {
	ProductID string
	// ConfigurationRevision is the exact revision admitted with the unit; zero
	// means the product had no configuration then.
	ConfigurationRevision int
	Repositories          []string
	Settings              map[string]string
	Knowledge             []ProductKnowledge
}

// ProductKnowledge is one recorded claim, with the scope and provenance that
// say how far it was trusted to travel.
type ProductKnowledge struct {
	ID         string
	Scope      string
	Provenance string
	Statement  string
}
