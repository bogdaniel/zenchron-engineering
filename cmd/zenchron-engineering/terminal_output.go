package main

// The terminal-safety boundary for dynamic text.
//
// Almost everything this CLI prints is its own words. What is not - a stage id,
// a role, a dependency name, an independence peer, a validation reason, a forge
// error, an issue objective - originated with a model, a provider or a
// third-party source, and #120 deliberately made the WORST of that material
// inspectable: a refused planning attempt exists to show an operator exactly
// what a model proposed when the machine refused it.
//
// That is the material most likely to contain something malformed or hostile,
// and a terminal is not an inert display. An ESC in a stage id is a control
// sequence: it can clear the screen, move the cursor, overwrite the line above,
// or repaint a refusal as an approval. Nothing upstream prevents it - the
// durable bounding path is ToValidUTF8 plus a byte truncation, and ESC, CSI,
// OSC and CR are all perfectly valid UTF-8.
//
// So the fix lives HERE, at the boundary where bytes become a terminal's input,
// and never in the durable record. The journal keeps the exact bounded proposal
// shape, because that is the evidence; the renderer refuses to execute it.

import (
	"fmt"
	"strings"
)

// terminalSafe renders one dynamic string with every control sequence made
// visible instead of executed.
//
// Normal Unicode is left exactly as it is: an operator reading an issue title
// in their own language should see it, and escaping beyond the control range
// would make honest text unreadable to protect against nothing.
func terminalSafe(text string) string {
	if strings.IndexFunc(text, isTerminalControl) < 0 {
		return text
	}
	var safe strings.Builder
	safe.Grow(len(text))
	for _, character := range text {
		if isTerminalControl(character) {
			// Shown the way Go shows it, so what an operator reads is what the
			// record holds rather than a silent deletion.
			fmt.Fprintf(&safe, "\\x%02x", character)
			continue
		}
		safe.WriteRune(character)
	}
	return safe.String()
}

// isTerminalControl reports whether a rune drives the terminal rather than
// appearing in it.
//
// C1 is included, not only C0 and DEL: U+009B is CSI on its own, so a filter
// that stopped at U+001F would leave the introducer it was written to stop.
func isTerminalControl(character rune) bool {
	return character < 0x20 || character == 0x7f || (character >= 0x80 && character <= 0x9f)
}

// terminalSafeList is terminalSafe over a list, for the joined renderings.
func terminalSafeList(values []string) []string {
	safe := make([]string, 0, len(values))
	for _, value := range values {
		safe = append(safe, terminalSafe(value))
	}
	return safe
}
