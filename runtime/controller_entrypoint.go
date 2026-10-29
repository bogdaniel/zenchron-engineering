package runtime

// THE ONE PUBLIC PATH ENTRYPOINT, and why it is a projection rather than a
// second copy of anything.
//
// `controller build-adopted` and succession together keep exactly one durable
// fact current: which generation under <controllerRoot> governs. Both already
// repair a pointer that follows that fact - StableEntrypointName, "current" -
// but that pointer sits inside the runtime's own state, not on the operator's
// PATH. Nothing before this file connected the two, so an operator's shell
// could resolve a copied binary that adoption/succession had already moved
// past, and nothing would ever tell them.
//
// The fix is not a second update mechanism. It is a symlink chain:
//
//	PATH entry -> <controllerRoot>/current/zenchron-engineering -> generation
//
// and the only thing this file adds is code that can (a) tell whether a PATH
// entry is that chain or something else, and (b) make it that chain without
// deleting whatever it replaces. Nothing here decides which generation is
// current - that authority already exists and is untouched.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EntrypointExecutableName is the one operator-facing command name every PATH
// lookup in this file searches for. It is a constant, not a derived value,
// because a migration compares candidates found under different PATH
// directories against each other, and deriving the name twice is how the two
// derivations come to disagree.
const EntrypointExecutableName = "zenchron-engineering"

// EntrypointCandidate is one PATH directory that holds a file named
// EntrypointExecutableName.
type EntrypointCandidate struct {
	Dir  string
	Path string
}

// EntrypointDiagnosis is the read-only picture both `autonomy doctor` and
// `controller install` start from: what a shell resolves today, and whether
// that resolution is the canonical, succession-following chain.
type EntrypointDiagnosis struct {
	// CanonicalTarget is <controllerRoot>/current/<EntrypointExecutableName>.
	// It is never a pinned generation directory: a symlink to one goes stale
	// the moment succession advances "current", which is the exact defect
	// this diagnosis exists to catch.
	CanonicalTarget string
	// Candidates is every PATH directory holding the executable, in PATH
	// order - the order a shell actually searches.
	Candidates []EntrypointCandidate
	// Winner is the candidate a shell resolves: the first one. Nil means
	// nothing named EntrypointExecutableName is on PATH at all.
	Winner *EntrypointCandidate
	// Canonical reports whether Winner is ITSELF a symlink whose own,
	// unresolved target is exactly CanonicalTarget. A symlink aimed at a
	// pinned generation directly is not canonical even if it happens to
	// resolve to the same file today: it stops following adoption the moment
	// succession moves "current".
	Canonical bool
	// ReachesAdopted reports whether Winner, fully resolved, names the same
	// file CanonicalTarget resolves to right now. It can be true while
	// Canonical is false (a pinned symlink that happens to name today's
	// generation) and false while Canonical is true ("current" itself is
	// broken).
	ReachesAdopted bool
	// AuthorityConsistent reports whether the durable controller authority -
	// the same subject governsNow and describeProjection already consult, and
	// the one thing this file must never re-derive a second opinion of - agrees
	// that CanonicalTarget's "current" pointer names the generation presently
	// governing. False means the projection is a repairable pointer that has
	// drifted from authority: filesystem readability of "current" is not
	// proof of what it should point at, so a caller must refuse rather than
	// treat the projection as healthy. Repairing it is the existing controller
	// law's job (re-adoption, succession) and never this file's.
	AuthorityConsistent bool
	// Detail explains a false Canonical, ReachesAdopted or AuthorityConsistent.
	// Empty means nothing needs explaining.
	Detail string
}

// Shadowed returns every PATH candidate other than the one a shell resolves -
// the entries a diagnosis must name rather than silently ignore. Nil means
// there is nothing shadowing the winner, including when there is no winner.
func (d EntrypointDiagnosis) Shadowed() []EntrypointCandidate {
	if d.Winner == nil || len(d.Candidates) < 2 {
		return nil
	}
	shadowed := make([]EntrypointCandidate, 0, len(d.Candidates)-1)
	for _, candidate := range d.Candidates {
		if candidate.Path != d.Winner.Path {
			shadowed = append(shadowed, candidate)
		}
	}
	return shadowed
}

// DiscoverEntrypointCandidates scans a $PATH-shaped string in order and
// returns every directory holding a file named EntrypointExecutableName.
// Directories are considered at most once, first occurrence wins, exactly as
// a shell's own PATH search never revisits a directory.
func DiscoverEntrypointCandidates(pathValue string) []EntrypointCandidate {
	var candidates []EntrypointCandidate
	seen := make(map[string]bool)
	for _, dir := range filepath.SplitList(pathValue) {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		path := filepath.Join(dir, EntrypointExecutableName)
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			continue
		}
		candidates = append(candidates, EntrypointCandidate{Dir: dir, Path: path})
	}
	return candidates
}

// DiagnoseEntrypoint answers what a shell would resolve right now for
// pathValue, and whether that resolution is the canonical chain through
// <controllerRoot>/current AND whether that chain agrees with durable
// controller authority. It changes nothing: every filesystem fact is read
// from what an operator's own shell would read, and the authority fact is
// read from the same subject governsNow and describeProjection already
// consult - never re-derived from the projection itself.
func DiagnoseEntrypoint(pathValue, controllerRoot string, authority currentAuthorityReader) EntrypointDiagnosis {
	diagnosis := EntrypointDiagnosis{
		CanonicalTarget: filepath.Join(controllerRoot, StableEntrypointName, EntrypointExecutableName),
		Candidates:      DiscoverEntrypointCandidates(pathValue),
	}
	if consistent, detail := entrypointAuthorityConsistency(controllerRoot, authority); consistent {
		diagnosis.AuthorityConsistent = true
	} else {
		diagnosis.Detail = appendDetail(diagnosis.Detail, detail)
	}
	if len(diagnosis.Candidates) > 0 {
		winner := diagnosis.Candidates[0]
		diagnosis.Winner = &winner
	}
	if diagnosis.Winner == nil {
		diagnosis.Detail = appendDetail(diagnosis.Detail, fmt.Sprintf("no %s was found on PATH", EntrypointExecutableName))
		return diagnosis
	}

	link, err := os.Readlink(diagnosis.Winner.Path)
	switch {
	case err != nil:
		diagnosis.Detail = appendDetail(diagnosis.Detail, fmt.Sprintf("%s is not a symlink; it is a detached copy, not the canonical entrypoint", diagnosis.Winner.Path))
	case filepath.Clean(resolveRelative(link, diagnosis.Winner.Dir)) == filepath.Clean(diagnosis.CanonicalTarget):
		diagnosis.Canonical = true
	default:
		diagnosis.Detail = appendDetail(diagnosis.Detail, fmt.Sprintf("%s is a symlink to %s, not the canonical %s", diagnosis.Winner.Path, link, diagnosis.CanonicalTarget))
	}

	resolvedWinner, winnerErr := filepath.EvalSymlinks(diagnosis.Winner.Path)
	resolvedCanonical, canonicalErr := filepath.EvalSymlinks(diagnosis.CanonicalTarget)
	switch {
	case canonicalErr != nil:
		diagnosis.Detail = appendDetail(diagnosis.Detail, "the adopted target "+diagnosis.CanonicalTarget+" could not be resolved: "+canonicalErr.Error())
	case winnerErr != nil:
		diagnosis.Detail = appendDetail(diagnosis.Detail, diagnosis.Winner.Path+" could not be resolved: "+winnerErr.Error())
	case resolvedWinner == resolvedCanonical:
		diagnosis.ReachesAdopted = true
	default:
		diagnosis.Detail = appendDetail(diagnosis.Detail, fmt.Sprintf("%s resolves to %s, which is not the adopted %s", diagnosis.Winner.Path, resolvedWinner, resolvedCanonical))
	}
	return diagnosis
}

// entrypointAuthorityConsistency answers the one question InstallCanonicalEntrypoint
// must ask before it ever trusts "current": does durable controller authority
// agree that the generation behind it is the one presently governing?
//
// It is the SAME comparison describeProjection already makes for `controller
// status` - the pointer's raw, unresolved readlink target against
// filepath.Dir(authority.Artifact) - because durable authority decides truth
// and "current" is only a repairable projection of it. A second, differently
// shaped comparison here would be a second authority mechanism, which #319
// explicitly must not create.
//
// NO AUTHORITY RECORDED YET IS A REFUSAL, not a pass. "current" is moved only
// after an activation or a re-adoption has established durable authority, so a
// readable "current" in a state directory that has never completed a
// transition is a pointer nothing sanctioned. Treating it as healthy would
// take authority from a filesystem symlink alone, which is the one thing #319
// forbids. A nil authority reader fails closed for the adjacent reason: the
// caller never wired one up, so the question could not be asked at all.
//
// AND THE PATH IS NOT THE ARTIFACT. Agreement on where the generation lives
// proves nothing about what now sits there: a replaced or modified executable
// at the same path satisfies every comparison above. The executable reached
// THROUGH the projection is therefore measured against the digest the
// governing binding attests, using the same measurement the adopted-build law
// already performs on publication. An authority that states no attested build
// cannot support that proof, so it is refused rather than excused.
func entrypointAuthorityConsistency(controllerRoot string, authority currentAuthorityReader) (consistent bool, detail string) {
	if authority == nil {
		return false, "no durable controller authority was consulted, so the \"current\" projection could not be proven to name the generation that actually governs"
	}
	current, found, err := authority.CurrentControllerAuthority()
	if err != nil {
		return false, "durable controller authority could not be read: " + err.Error()
	}
	pointer := filepath.Join(controllerRoot, StableEntrypointName)
	if !found {
		return false, fmt.Sprintf(
			"no durable controller authority governs this state directory, so %s names a generation nothing has adopted; adopt a controller (`controller build-adopted`) or re-adopt one before treating it as the public entrypoint",
			pointer)
	}
	if strings.TrimSpace(current.Artifact) == "" {
		return false, fmt.Sprintf(
			"durable controller authority (%s %s) names no artifact, so there is nothing %s can be proven to point at",
			current.Kind, current.Ref, pointer)
	}
	target, err := os.Readlink(pointer)
	if err != nil {
		return false, fmt.Sprintf("%s could not be read to compare against durable authority: %s", pointer, err.Error())
	}
	governs := filepath.Dir(current.Artifact)
	if target != governs {
		return false, fmt.Sprintf(
			"%s points at %s, but durable controller authority (%s %s) says %s governs; repair this through the existing controller authority (re-adoption or succession), not by installing over it",
			pointer, target, current.Kind, current.Ref, governs)
	}
	return entrypointArtifactIntegrity(pointer, current)
}

// entrypointArtifactIntegrity proves the executable reached through the
// projection is the one the governing binding attests, byte for byte.
//
// It measures THROUGH the pointer rather than at the recorded artifact path,
// even though the two are proven equal above: the chain a shell traverses is
// the thing being blessed, and measuring it is what makes the proof about the
// public entrypoint rather than about a path that happens to match.
func entrypointArtifactIntegrity(pointer string, current ControllerAuthority) (consistent bool, detail string) {
	build := current.Binding.Build
	if build == nil || !isSHA256Hex(build.BinarySHA256) {
		return false, fmt.Sprintf(
			"durable controller authority (%s %s) attests no build digest, so the executable behind %s cannot be proven to be the adopted one",
			current.Kind, current.Ref, pointer)
	}
	reached := filepath.Join(pointer, filepath.Base(current.Artifact))
	measured, err := measureExecutable(reached)
	if err != nil {
		return false, fmt.Sprintf(
			"the executable reached through %s could not be measured against durable authority: %s", pointer, err.Error())
	}
	if measured != build.BinarySHA256 {
		return false, fmt.Sprintf(
			"%s reaches an executable measuring %s, and durable controller authority (%s %s) attests %s; the adopted artifact has been replaced or modified, and this will not make it the public command",
			pointer, shortSHA(measured), current.Kind, current.Ref, shortSHA(build.BinarySHA256))
	}
	return true, ""
}

// resolveRelative joins a symlink's own target against the directory holding
// the link when the target is relative, exactly as the kernel would.
func resolveRelative(target, dir string) string {
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(dir, target)
}

func appendDetail(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

// DefaultEntrypointBinDir is where `controller install` places the canonical
// entrypoint when nothing named EntrypointExecutableName is already anywhere
// on PATH. It is user-local and needs no elevated privilege: the product
// invariant is one supported entrypoint, not one installed with sudo.
func DefaultEntrypointBinDir(home string) string {
	return filepath.Join(home, ".local", "bin")
}

// EntrypointInstallResult is what InstallCanonicalEntrypoint did, or found
// already true.
type EntrypointInstallResult struct {
	// Path is the PATH entry that now holds, or already held, the canonical
	// symlink.
	Path string
	// AlreadyCanonical reports that Path already resolved through
	// <controllerRoot>/current before this call; nothing was changed.
	AlreadyCanonical bool
	// RetiredPath is non-empty when a previous occupant of Path was moved
	// aside rather than deleted.
	RetiredPath string
	// OnPath reports whether Path's directory actually appears in pathValue.
	// False after a fresh install into binDir means the operator still has
	// to add it to their shell's PATH.
	OnPath bool
}

// InstallCanonicalEntrypoint makes a PATH entry the canonical symlink to
// <controllerRoot>/current/<EntrypointExecutableName>, retiring whatever
// currently occupies that entry rather than deleting it.
//
// It targets the WINNING PATH entry when one exists, so migrating a stale
// standalone binary converts the exact file a shell already resolves - the
// deterministic migration #319 requires, rather than a second install beside
// the stale one. Only when nothing named EntrypointExecutableName is on PATH
// at all does it fall back to binDir, which the caller must then confirm is
// itself on PATH.
//
// It never touches an entry other than the one it installs into: other
// occurrences on PATH are reported by DiagnoseEntrypoint's Shadowed, not
// silently modified, because guessing which of several stale copies to erase
// is exactly the silent behaviour #319 refuses.
//
// It refuses rather than installs when "current" has drifted from durable
// controller authority (DiagnoseEntrypoint's AuthorityConsistent). A drifted
// projection is readable - os.Stat on it succeeds - but readable is not the
// same fact as authoritative, and blessing it as the public entrypoint would
// be exactly the silent trust #319's review refused. The fix for drift is the
// existing controller law (re-adoption, succession), never a repair
// performed here.
func InstallCanonicalEntrypoint(pathValue, binDir, controllerRoot string, authority currentAuthorityReader) (EntrypointInstallResult, error) {
	diagnosis := DiagnoseEntrypoint(pathValue, controllerRoot, authority)
	if !diagnosis.AuthorityConsistent {
		return EntrypointInstallResult{}, fmt.Errorf(
			"the canonical target %s cannot be trusted: %s", diagnosis.CanonicalTarget, diagnosis.Detail)
	}
	target := diagnosis.CanonicalTarget
	if _, err := os.Stat(target); err != nil {
		return EntrypointInstallResult{}, fmt.Errorf(
			"the adopted target %s is not readable: %w; adopt a controller (`controller build-adopted`) before installing the entrypoint", target, err)
	}

	var link string
	if diagnosis.Winner != nil {
		link = diagnosis.Winner.Path
	} else {
		if strings.TrimSpace(binDir) == "" {
			return EntrypointInstallResult{}, fmt.Errorf(
				"no %s was found on PATH and no bin directory was given to install one into", EntrypointExecutableName)
		}
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return EntrypointInstallResult{}, err
		}
		link = filepath.Join(binDir, EntrypointExecutableName)
	}

	result := EntrypointInstallResult{Path: link, OnPath: onPath(pathValue, filepath.Dir(link))}
	if diagnosis.Winner != nil && diagnosis.Canonical {
		result.AlreadyCanonical = true
		return result, nil
	}

	if existing, err := os.Lstat(link); err == nil {
		if existing.IsDir() {
			return EntrypointInstallResult{}, fmt.Errorf(
				"%s is a directory; the canonical entrypoint is a link and this will not delete an operator's directory to become one", link)
		}
		retired, err := retiredSiblingPath(link)
		if err != nil {
			return EntrypointInstallResult{}, err
		}
		// THE OPERATOR'S COMMAND IS NEVER ABSENT, not even for an instant.
		//
		// Renaming the existing entry away first and then installing the new
		// link would leave the public PATH location EMPTY if the second step
		// failed - a failed install that deletes the working command, which is
		// worse than not installing at all. replaceSymlinkAtomically exists so
		// a reader sees old-or-new and never nothing, and that guarantee is
		// only worth anything if the old one is still there to be seen.
		//
		// So the old artifact is preserved BESIDE itself first and the public
		// path is replaced atomically afterwards. A hard link keeps a detached
		// binary without copying it; a symlink is recreated by value.
		if err := preserveRetiredEntry(link, existing, retired); err != nil {
			return EntrypointInstallResult{}, fmt.Errorf("the existing %s could not be retired: %w", link, err)
		}
		if err := replaceSymlinkAtomically(target, link); err != nil {
			// The public path still holds what it held before. Remove the
			// retirement copy so a later attempt is not confused by a
			// half-finished migration.
			_ = os.Remove(retired)
			return EntrypointInstallResult{}, err
		}
		result.RetiredPath = retired
		return result, nil
	}

	if err := replaceSymlinkAtomically(target, link); err != nil {
		return EntrypointInstallResult{}, err
	}
	return result, nil
}

// preserveRetiredEntry copies the existing entrypoint to retired WITHOUT
// disturbing the original.
//
// A symlink is recreated by value rather than followed, so what is kept is the
// operator's previous pointer rather than a second name for its target. A
// regular file is hard-linked, which keeps a detached binary - typically tens
// of megabytes - without copying its bytes and without depending on the
// original surviving the replacement that follows.
func preserveRetiredEntry(link string, existing os.FileInfo, retired string) error {
	if existing.Mode()&os.ModeSymlink != 0 {
		previous, err := os.Readlink(link)
		if err != nil {
			return err
		}
		return os.Symlink(previous, retired)
	}
	return os.Link(link, retired)
}

// retiredSiblingPath names an unused ".pre-canonical" sibling of link, so a
// detached executable is kept beside where it used to be rather than deleted
// or dropped somewhere an operator would not think to look.
func retiredSiblingPath(link string) (string, error) {
	base := link + ".pre-canonical"
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		return base, nil
	}
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an unused name to retire %s under", link)
}

// onPath reports whether dir appears in a $PATH-shaped string.
func onPath(pathValue, dir string) bool {
	for _, entry := range filepath.SplitList(pathValue) {
		if filepath.Clean(strings.TrimSpace(entry)) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
