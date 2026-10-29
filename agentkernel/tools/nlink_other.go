//go:build !unix

package tools

import "io/fs"

// hardLinked cannot see link counts through fs.FileInfo off unix, so hard
// links are not refused there: a hard link to repository metadata inside a
// granted root aliases it. This is a documented limitation of the
// development-grade guards (docs/spec/capabilities-v0.1.md §4.1).
func hardLinked(fs.FileInfo) bool { return false }
