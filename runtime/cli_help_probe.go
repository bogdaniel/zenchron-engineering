package runtime

// Help-text advertised-flag parsing for CLIAgentProvider.probeCapability.
//
// Extracted from cli_agent.go (which sits at its frozen file-size ceiling)
// rather than appended to it: this is already a cohesive, self-contained unit
// - deciding whether a CLI's own --help output proves it supports a flag and
// a specific value for it - with no dependency on anything else in that file
// beyond the capability check that calls it.

import "strings"

// cliFlagChoice is one value a flag must advertise.
type cliFlagChoice struct{ Flag, Value string }

// advertisesChoice reports whether help output offers a value AS A CHOICE of a
// flag. The value must appear as a whole word within the flag's own description
// - the text from the flag name up to the next flag or blank line - so a
// mention of the word elsewhere in the help output proves nothing about the
// flag this adapter is about to pass.
//
// Only an occurrence that OPENS a help row counts (#322). Claude Code's help
// mentions `--output-format=stream-json` inside the descriptions of three other
// options before it reaches the --output-format row itself, so matching the
// first occurrence anywhere accepted `stream-json` from a sentence about a
// different flag - and the structured-progress probe would have passed against
// a binary whose --output-format no longer offered it.
func advertisesChoice(advertised, flag, value string) bool {
	for offset := 0; ; {
		index := strings.Index(advertised[offset:], flag)
		if index < 0 {
			return false
		}
		at := offset + index
		start := at + len(flag)
		if opensHelpRow(advertised, at, flag) && advertisesToken(flagDescription(advertised[start:]), value) {
			return true
		}
		offset = start
	}
}

// opensHelpRow reports whether the flag at index `at` is the option a help row
// describes: it sits in the option column - at most six columns of indentation,
// which covers commander's two and clap's two or six - optionally after a short
// alias such as `-c, `, and it is neither the prefix of a longer flag nor the
// `--flag=value` spelling prose uses. A wrapped description line is indented
// much deeper, and the real Claude help wraps one onto a line that begins
// `--output-format=stream-json)`.
func opensHelpRow(text string, at int, flag string) bool {
	lineStart := strings.LastIndexByte(text[:at], '\n') + 1
	prefix := strings.TrimLeft(text[lineStart:at], " ")
	indent := at - lineStart - len(prefix)
	if len(prefix) == 4 && prefix[0] == '-' && wordCharacter(prefix, 1) && prefix[2:] == ", " {
		prefix = ""
	}
	end := at + len(flag)
	return prefix == "" && indent <= 6 && !wordCharacter(text, end) && (end >= len(text) || text[end] != '=')
}

// flagDescription is the run of help text belonging to one flag: everything up
// to the next flag or the next blank line, whichever comes first.
func flagDescription(text string) string {
	// Normalized first: a CRLF help output would otherwise never match any of
	// the cuts below and the whole remainder would count as one flag's
	// description.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	end := len(text)
	cut := func(marker string) {
		if next := strings.Index(text, marker); next >= 0 && next < end {
			end = next
		}
	}
	cut("\n  -")
	cut("\n-")
	// clap indents long-only flags by six spaces. Without this the "possible
	// values" exception below can carry a description across the NEXT flag, and
	// the choice check then accepts a value that belongs to a different flag -
	// the exact association this function exists to require.
	cut("\n      --")
	cut("\n    --")
	// A blank line ends the description UNLESS the next block is this flag's
	// own value list. clap prints "Possible values:" as an indented block after
	// a blank line, so cutting at the blank line put the choices outside the
	// description and withheld a capability the binary advertises.
	if blank := strings.Index(text, "\n\n"); blank >= 0 && blank < end {
		rest := text[blank:]
		if values := strings.Index(rest, "ossible values"); values < 0 || (end-blank) < values {
			end = blank
		}
	}
	return text[:end]
}

// advertisesToken reports whether help output contains a token as a whole word.
// It deliberately does not assume quoting: a CLI may print its choices quoted,
// bracketed, comma-separated or bare, and all of those are word boundaries.
func advertisesToken(advertised, token string) bool {
	for offset := 0; ; {
		index := strings.Index(advertised[offset:], token)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(token)
		if !wordCharacter(advertised, start-1) && !wordCharacter(advertised, end) {
			return true
		}
		offset = start + 1
	}
}

func wordCharacter(text string, at int) bool {
	if at < 0 || at >= len(text) {
		return false
	}
	c := text[at]
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
