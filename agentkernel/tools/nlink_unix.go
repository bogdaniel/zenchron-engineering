//go:build unix

package tools

import (
	"io/fs"
	"syscall"
)

// hardLinked reports whether a file has more than one name. A second name can
// alias .git/config with no symlink anywhere in the path. Unknown is treated
// as linked, so the guard fails closed.
func hardLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || st.Nlink > 1
}
