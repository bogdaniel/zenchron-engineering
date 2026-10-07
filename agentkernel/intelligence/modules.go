package intelligence

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// parseGoMod reads only the module and go directives. The standard library has
// no go.mod parser and x/mod is a dependency this module may not take; require
// and replace blocks do not affect structural extraction because nothing
// outside the workspace is indexed.
func parseGoMod(content []byte) (modPath, goDirective string, err error) {
	for _, line := range strings.Split(string(content), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "module":
			modPath = fields[1]
			if unq, uerr := strconv.Unquote(modPath); uerr == nil {
				modPath = unq
			}
		case "go":
			goDirective = fields[1]
		}
	}
	if modPath == "" {
		return "", "", fmt.Errorf("no module directive")
	}
	return modPath, goDirective, nil
}

// moduleFor returns the module whose directory is the nearest ancestor of dir.
func moduleFor(modules []Module, dir string) (Module, bool) {
	best, found := Module{}, false
	for _, m := range modules {
		if isAncestorOrEqual(m.Dir, dir) && (!found || len(m.Dir) > len(best.Dir) || best.Dir == ".") {
			best, found = m, true
		}
	}
	return best, found
}

// importPathFor is the import path of the package in dir. A directory outside
// every module gets a "_/"-prefixed path that no real import can name.
func importPathFor(modules []Module, dir string) (string, bool) {
	m, ok := moduleFor(modules, dir)
	if !ok {
		return path.Join("_", dir), false
	}
	if m.Dir == dir {
		return m.Path, true
	}
	rel := dir
	if m.Dir != "." {
		rel = strings.TrimPrefix(dir, m.Dir+"/")
	}
	return m.Path + "/" + rel, true
}

// isGoPackageDir mirrors the go tool: testdata, vendor and directories
// starting with "." or "_" never hold packages of the module.
func isGoPackageDir(dir string) bool {
	if dir == "." {
		return true
	}
	for _, elem := range strings.Split(dir, "/") {
		if elem == "testdata" || elem == "vendor" || strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") {
			return false
		}
	}
	return true
}

// goDirs groups the manifest's Go source files by package directory.
func goDirs(m Manifest) map[string][]string {
	dirs := map[string][]string{}
	for _, f := range m.Files {
		name := path.Base(f.Path)
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		dir := path.Dir(f.Path)
		if isGoPackageDir(dir) {
			dirs[dir] = append(dirs[dir], name)
		}
	}
	for _, names := range dirs {
		slices.Sort(names)
	}
	return dirs
}

// moduleKey changes whenever anything about the module governing dir changes:
// a go.mod added, removed, moved or edited.
func moduleKey(modules []Module, dir string) string {
	m, ok := moduleFor(modules, dir)
	if !ok {
		return ""
	}
	return m.Dir + "\x00" + m.Path + "\x00" + m.GoModDigest
}
