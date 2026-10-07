package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// ToolchainProber is the capability of a provider whose commands run in an
// environment the host cannot see - a container, a remote sandbox - to answer
// which required tools that environment cannot resolve. Gate (a):
// OpenAIProvider probes its pinned container, and the composition root's
// candidate-bound wrapper forwards that probe (#522).
//
// A provider that does not implement it runs commands in this process's
// environment, so the host answers from the declared toolchain itself.
type ToolchainProber interface {
	MissingTools(ctx context.Context, required []string) []string
}

var _ ToolchainProber = OpenAIProvider{}

// hostMissingTools is the declared required tools the supervisor's own
// environment cannot resolve, in declaration order.
func hostMissingTools(toolchain ToolchainConfig) []string {
	return missingToolchainTools(toolchain, OSCommandExecutor{}.LookPath)
}

// missingToolchainTools is the one rule for resolving a declared toolchain.
//
// Resolution happens against the BROKERED path when one is declared. An
// operator who declares required tools without a path is asking about the
// inherited environment, which is answered honestly rather than refused: the
// two halves of the toolchain are independently useful.
func missingToolchainTools(toolchain ToolchainConfig, lookPath func(string) error) []string {
	var missing []string
	for _, tool := range toolchain.RequiredTools {
		if tool = strings.TrimSpace(tool); tool == "" {
			continue
		}
		if !toolchainResolves(toolchain, tool, lookPath) {
			missing = append(missing, tool)
		}
	}
	return missing
}

func toolchainResolves(toolchain ToolchainConfig, tool string, lookPath func(string) error) bool {
	if len(toolchain.Path) == 0 {
		return lookPath(tool) == nil
	}
	for _, dir := range toolchain.Path {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		// An absolute candidate confines lookup to the declared directory.
		candidate, err := filepath.Abs(filepath.Join(dir, tool))
		if err != nil {
			continue
		}
		if resolvesDeclaredExecutable(candidate) {
			return true
		}
	}
	return false
}

// resolvesDeclaredExecutable checks the declared toolchain's executable files.
// On Unix, retain the file-mode readiness check: LookPath additionally uses
// access syscalls on some platforms, which can be denied by the supervisor's
// sandbox even when the worker's execution environment permits the tool.
// Windows requires native extension lookup (PATHEXT), not Unix mode bits.
func resolvesDeclaredExecutable(candidate string) bool {
	if goruntime.GOOS == "windows" {
		_, err := exec.LookPath(candidate)
		return err == nil
	}
	info, err := os.Stat(candidate)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
