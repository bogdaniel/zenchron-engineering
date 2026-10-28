package domain

import "regexp"

// DeclaredAcceptanceFeature is the operator-declared feature acceptance this
// plan carries, or "" when it carries none.
func (p EngineeringPlan) DeclaredAcceptanceFeature() string {
	for _, stage := range p.Stages {
		if stage.AcceptanceFeature != "" {
			return stage.AcceptanceFeature
		}
	}
	return ""
}

var acceptanceFeaturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidAcceptanceFeature reports whether id is usable as a feature name and as
// the file name browser-acceptance/<id>.txt.
func ValidAcceptanceFeature(id string) bool { return acceptanceFeaturePattern.MatchString(id) }
