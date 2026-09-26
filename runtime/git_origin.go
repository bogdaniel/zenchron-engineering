package runtime

// WHO ORIGINATED a governed Git operation.
//
// #241 answers what a command intends, #257 answers which resource it lands
// on, and neither answers the question every conclusion drawn from a refusal
// silently assumes: which autonomous actor issued it. Run
// run-0bd3b7ab3a773b88566570b7a99a7a0e made 63 refusals and the record could
// not say that ZERO of them were the model's. Three readings were published
// from that record and all three were wrong; each was corrected only by
// correlating the provider's own session transcript by hand.
//
// THE ORIGIN IS ESTABLISHED FROM EXECUTION TOPOLOGY, NEVER FROM THE COMMAND.
// `git log --since=7.days --diff-filter=A --name-only` does not look like
// something a worker types, and that observation is a hypothesis rather than
// evidence: a model can type it and a provider runtime can issue something
// that looks hand-written. Reading origin out of argv would rebuild the exact
// mistake this file exists to end, with a confident label on top of it.
//
// CAUSATION IS NOT PROVENANCE. A model that runs `go test ./...` causes the
// test binaries that follow, and the commits those binaries attempt are not
// the model's Git operations. The attribution names the actor that DIRECTLY
// originated the operation, never the ancestor that caused that actor to
// exist, because "the model attempted 51 commits" is precisely the false
// conclusion that had to be undone by forensics.
//
// WHAT THE ANCHOR IS, and why it is not read from the environment: the
// controller knows its own process identity before any provider exists, and
// the shim it writes carries that identity. Everything below is measured
// against a number the runtime established itself. `CLAUDE_PID` and its
// neighbours are in the environment of every process the provider spawns,
// including the ones this file must tell apart, so they answer nothing.
//
// NOTHING HERE CHANGES WHAT IS PERMITTED. The attribution is recorded beside
// a decision that was already made; no arm of git_authority.go consults it,
// and an `unknown` origin is never more permissive than a known one. An
// origin that widened authority would be a caller-shaped escape hatch, which
// is the counter-law git_target.go states for the resource question and which
// applies here with more force: ancestry is observable, and observable is not
// the same as unforgeable.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// GitActorOrigin is the actor that directly originated a governed Git
// operation. The vocabulary is deliberately generic and provider-neutral: a
// provider adapter may later prove a finer subsystem identity, and the
// evidence schema must not have to learn one vendor's internals to record it.
type GitActorOrigin string

const (
	// GitOriginModelTool is Git invoked by the model's own tool execution
	// context - the process the provider creates to run one tool call.
	GitOriginModelTool GitActorOrigin = "model_tool"
	// GitOriginProviderRuntime is Git invoked by the provider's own machinery:
	// repository probing, checkpointing, restore. The model chose none of it.
	GitOriginProviderRuntime GitActorOrigin = "provider_runtime"
	// GitOriginZenchronRuntime is Git invoked by this controller.
	GitOriginZenchronRuntime GitActorOrigin = "zenchron_runtime"
	// GitOriginWorkloadSubprocess is Git invoked by a program running inside
	// the workload - a test binary, a build system, a package script. It is
	// its own actor precisely because it is NOT the model: the model may have
	// started the program, and the program chose the Git command.
	GitOriginWorkloadSubprocess GitActorOrigin = "workload_subprocess"
	// GitOriginUnknown is an origin the runtime could not establish. It is the
	// honest answer and the only safe default: a guess recorded as evidence is
	// worse than an absence, because the absence is visible to the reader.
	GitOriginUnknown GitActorOrigin = "unknown"
)

// GitOriginAnchor is what the runtime established BEFORE the provider process
// existed. It travels to the broker through the generated shim, which is
// runtime-owned, so nothing a provider can set participates in the answer.
type GitOriginAnchor struct {
	// ControllerPID is the controller that prepared this attempt's guard.
	// Zero means no anchor was recorded, which yields unknown rather than a
	// fallback: a boundary that guessed its own identity would be attributing
	// against whatever process happened to be there.
	ControllerPID int
	// ToolCallDepth is how many processes a provider kind places between
	// itself and one tool call's command, MEASURED for that kind rather than
	// assumed. Zero means undeclared, and an undeclared provider never yields
	// model_tool - it yields workload_subprocess or unknown, because claiming
	// the model issued something it may not have is the one error whose cost
	// this whole issue documents.
	ToolCallDepth int
}

// ToolCallDepthFor reports the measured tool-call depth of a provider kind.
//
// THIS IS PROVIDER CAPABILITY METADATA, NOT A UNIVERSAL PROCESS-DEPTH LAW.
// Where a provider places one tool call relative to itself is a property of
// that provider, and `claude_code = 1` is an observation about Claude Code.
// Hardening it into "three hops is the model" for every adapter would report
// the machinery of a provider that shells out internally as model behaviour,
// which is the original defect reached through a newer mechanism. A kind whose
// topology nobody measured declares nothing, and a kind that declares nothing
// can never produce model_tool.
//
// claude_code was measured directly: a headless session whose only model
// output was the word "ok" issued eight Git commands, every one of them a
// DIRECT child of the provider process, while a session told to run one Git
// command produced it one process lower, under the shell the provider creates
// for a Bash tool call. That is the whole basis for the number, and it is why
// the other kinds are absent rather than guessed at.
func ToolCallDepthFor(kind string) int {
	if kind == AgentKindClaudeCode {
		return 1
	}
	return 0
}

// GitActorAttribution is the origin together with HOW it was established, so a
// reviewer can audit the derivation instead of trusting the label.
type GitActorAttribution struct {
	Origin GitActorOrigin `json:"origin"`
	Basis  string         `json:"basis,omitempty"`
}

// maxAncestryWalk bounds the climb. A chain longer than this is a workload
// nesting programs deeply, and the answer at that point is already
// workload_subprocess; the bound exists so an unreadable or cyclic process
// table cannot hold the broker.
const maxAncestryWalk = 32

// EstablishGitActorOrigin walks from this process to the controller and reads
// the origin off the DISTANCE between them.
//
// The shape of the chain is what carries the answer:
//
//	controller -> broker                              this controller's own Git
//	controller -> provider -> broker                  the provider's machinery
//	controller -> provider -> tool context -> broker  one model tool call
//	anything deeper                                   a program in the workload
//
// parentOf is a parameter so the table above can be driven exactly in a test,
// including the chains no fixture can produce on demand.
func EstablishGitActorOrigin(anchor GitOriginAnchor, self int, parentOf func(int) (int, bool)) GitActorAttribution {
	if anchor.ControllerPID <= 0 {
		return GitActorAttribution{Origin: GitOriginUnknown,
			Basis: "no controller identity was recorded for this attempt"}
	}
	pid := self
	for hops := 1; hops <= maxAncestryWalk; hops++ {
		parent, ok := parentOf(pid)
		if !ok {
			return GitActorAttribution{Origin: GitOriginUnknown,
				Basis: fmt.Sprintf("the parent of process %d could not be read", pid)}
		}
		if parent == anchor.ControllerPID {
			return attributionAtDepth(hops, anchor)
		}
		// Reaching init means the controller is not an ancestor at all. That
		// is a real condition rather than an error - a crashed and restarted
		// controller leaves an attempt whose guard names a pid nobody is -
		// and it is unknown, not a reason to start inferring.
		if parent <= 1 {
			return GitActorAttribution{Origin: GitOriginUnknown,
				Basis: fmt.Sprintf("controller %d is not an ancestor of this invocation", anchor.ControllerPID)}
		}
		pid = parent
	}
	return GitActorAttribution{Origin: GitOriginUnknown,
		Basis: fmt.Sprintf("the controller was not reached within %d ancestors", maxAncestryWalk)}
}

// attributionAtDepth maps the measured distance to the actor.
func attributionAtDepth(hops int, anchor GitOriginAnchor) GitActorAttribution {
	basis := fmt.Sprintf("process ancestry: %d hop(s) to controller %d", hops, anchor.ControllerPID)
	switch {
	case hops == 1:
		return GitActorAttribution{Origin: GitOriginZenchronRuntime, Basis: basis}
	case hops == 2:
		return GitActorAttribution{Origin: GitOriginProviderRuntime, Basis: basis}
	case anchor.ToolCallDepth > 0 && hops == 2+anchor.ToolCallDepth:
		return GitActorAttribution{Origin: GitOriginModelTool,
			Basis: basis + fmt.Sprintf("; the provider places one tool call %d hop(s) below itself", anchor.ToolCallDepth)}
	default:
		// DELIBERATELY NOT model_tool. A program the model started is not the
		// model, and an undeclared provider kind has nothing to place a tool
		// call at, so both land here rather than being promoted.
		return GitActorAttribution{Origin: GitOriginWorkloadSubprocess, Basis: basis}
	}
}

// processParent reads one process's parent from the operating system.
//
// It shells out because Go exposes no portable call for another process's
// parent, and it names an absolute path because the broker runs with the
// guard directory first on PATH: resolving `ps` through a search path the
// provider's environment contributed to would let the answer be chosen by the
// thing being attributed.
func processParent(pid int) (int, bool) {
	for _, program := range []string{"/bin/ps", "/usr/bin/ps"} {
		if _, err := os.Stat(program); err != nil {
			continue
		}
		out, err := exec.Command(program, "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, false
		}
		parent, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
		if convErr != nil {
			return 0, false
		}
		return parent, true
	}
	return 0, false
}
