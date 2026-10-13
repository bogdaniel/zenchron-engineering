package controlplane

import (
	"strconv"
	"strings"
)

// runsFilter is shared by the read API and the temporary legacy renderer.
// Source identity and held-material semantics have one authoritative home.
type runsFilter struct {
	Status string
	Source string
}

func runsFilterFromQuery(values map[string][]string) runsFilter {
	get := func(key string) string {
		if v, ok := values[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	return runsFilter{Status: get("status"), Source: get("source")}
}

// matches reports whether one run satisfies the filter. "held" is not a
// Disposition: it asks Run.Held directly, independent of which terminal
// disposition a held run settled into.
func (f runsFilter) matches(s Run) bool {
	if f.Status != "" {
		if f.Status == "held" {
			if !s.Held {
				return false
			}
		} else if string(s.Disposition) != f.Status {
			return false
		}
	}
	if f.Source != "" && !matchesSource(s, f.Source) {
		return false
	}
	return true
}

// matchesSource accepts either "owner/repo#123" or a bare issue number: an
// operator watching one issue rarely remembers which repository it was filed
// against and should not have to type it to filter on it.
func matchesSource(s Run, source string) bool {
	if repo, issue, ok := strings.Cut(source, "#"); ok {
		number, err := strconv.Atoi(strings.TrimSpace(issue))
		return err == nil && number > 0 && s.Repository == repo && s.Issue == number
	}
	if number, err := strconv.Atoi(source); err == nil {
		return number > 0 && s.Issue == number
	}
	return false
}
