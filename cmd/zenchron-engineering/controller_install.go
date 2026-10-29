package main

// `controller install`: establishes, or repairs, the ONE public PATH
// entrypoint (#319).
//
// It decides nothing about which generation governs - that is
// adoption/succession's authority, untouched here - and it never installs a
// second copy beside a stale one. It converts whichever zenchron-engineering
// a shell already resolves into a symlink through
// <controllerRoot>/current/zenchron-engineering, retiring the previous
// occupant beside itself rather than deleting it. Only when nothing resolves
// on PATH at all does it create a fresh entry, under --bin-dir.
//
// The whole decision procedure is runtime.InstallCanonicalEntrypoint; this
// file is wiring, flags and the printed report.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const installUsage = `usage: zenchron-engineering controller install [--bin-dir <dir>] [--config <path>]

Establish the one canonical PATH entrypoint: a symlink that resolves through
~/.zenchron-adopted-controller/current to whichever generation
adoption/succession currently governs.

If a zenchron-engineering executable already resolves on PATH, that exact
location is converted in place; a previous occupant is renamed with a
.pre-canonical suffix beside itself rather than deleted. If nothing resolves
on PATH yet, the entrypoint is created under --bin-dir (default ~/.local/bin).`

func controllerInstall(args []string, stdout io.Writer) (int, error) {
	binDir, config, err := parseInstallFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	if binDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return runtime.ExitFailed, err
		}
		binDir = runtime.DefaultEntrypointBinDir(home)
	}
	pathEnv := os.Getenv("PATH")
	root := controllerRoot()

	// The durable authority this installs a projection of is read through the
	// SAME store `controller status` and succession already use: a second,
	// differently-sourced opinion here would be the second authority
	// mechanism #319 explicitly refuses.
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitFailed, err
	}
	loaded, err := runtime.LoadConfig(config, cwd)
	if err != nil {
		return runtime.ExitFailed, err
	}
	store, err := runtime.OpenSQLiteOperationStore(loaded.StateDir)
	if err != nil {
		return runtime.ExitFailed, err
	}
	defer store.Close()

	result, err := runtime.InstallCanonicalEntrypoint(pathEnv, binDir, root, store)
	if err != nil {
		return runtime.ExitFailed, err
	}

	pointer := filepath.Join(root, runtime.StableEntrypointName)
	fmt.Fprintf(stdout, "entrypoint:  %s\n", result.Path)
	if result.AlreadyCanonical {
		fmt.Fprintf(stdout, "status:      already canonical, resolving through %s\n", pointer)
	} else {
		if result.RetiredPath != "" {
			fmt.Fprintf(stdout, "retired:     %s (kept, not deleted)\n", result.RetiredPath)
		}
		fmt.Fprintf(stdout, "status:      now resolves through %s\n", pointer)
	}
	if !result.OnPath {
		fmt.Fprintf(stdout, "\nWARNING: %s is not on PATH. Add it to your shell profile, e.g.\n  export PATH=\"%s:$PATH\"\n",
			filepath.Dir(result.Path), filepath.Dir(result.Path))
	}

	diagnosis := runtime.DiagnoseEntrypoint(pathEnv, root, store)
	if shadowed := diagnosis.Shadowed(); len(shadowed) > 0 {
		fmt.Fprintf(stdout, "\nshadowed entries still on PATH (not modified):\n")
		for _, candidate := range shadowed {
			fmt.Fprintf(stdout, "  %s\n", candidate.Path)
		}
		fmt.Fprintf(stdout, "these will not win a PATH lookup today, but removing them avoids future confusion.\n")
	}
	return runtime.ExitCompleted, nil
}

func parseInstallFlags(args []string) (binDir, config string, err error) {
	for len(args) > 0 {
		switch args[0] {
		case "--bin-dir":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", "", fmt.Errorf("--bin-dir requires a non-empty path\n\n%s", installUsage)
			}
			if binDir != "" {
				return "", "", fmt.Errorf("--bin-dir was given more than once\n\n%s", installUsage)
			}
			binDir, args = args[1], args[2:]
		case "--config":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", "", fmt.Errorf("--config requires a non-empty path\n\n%s", installUsage)
			}
			if config != "" {
				return "", "", fmt.Errorf("--config was given more than once\n\n%s", installUsage)
			}
			config, args = args[1], args[2:]
		default:
			return "", "", fmt.Errorf("controller install does not accept %q; it takes --bin-dir and --config\n\n%s", args[0], installUsage)
		}
	}
	return binDir, config, nil
}
