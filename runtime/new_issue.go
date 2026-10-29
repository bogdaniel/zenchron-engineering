package runtime

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The bounds keep one request line inside the ordinary control ceiling
// (maxControlRequestBytes) even after JSON escaping of typical prose.
const (
	MaxNewIssueTitleBytes = 200
	MaxNewIssueBodyBytes  = 3000
)

// NewIssue is an operator's own words for an issue to open. It is authored by
// the authenticated operator, never read from a forge, so it is not
// UntrustedText; it is still validated because it crosses the HTTP boundary.
type NewIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Validate refuses a blank, oversized or non-UTF-8 draft.
func (n NewIssue) Validate() error {
	if strings.TrimSpace(n.Title) == "" || strings.TrimSpace(n.Body) == "" {
		return errors.New("an issue needs both a title and a description")
	}
	if len(n.Title) > MaxNewIssueTitleBytes || len(n.Body) > MaxNewIssueBodyBytes {
		return fmt.Errorf("an issue title is at most %d bytes and its description at most %d", MaxNewIssueTitleBytes, MaxNewIssueBodyBytes)
	}
	if !utf8.ValidString(n.Title) || !utf8.ValidString(n.Body) || strings.ContainsAny(n.Title, "\r\n") {
		return errors.New("an issue title is one line of valid text")
	}
	return nil
}

// ForgeBody is the description plus a footer naming where the issue came from
// and who asked, so the origin of an issue the product opened is never in doubt.
func (n NewIssue) ForgeBody(operator string) string {
	return strings.TrimRight(n.Body, "\n") + "\n\n---\nCreated from the Zenchron dashboard by operator `" + operator + "`."
}
