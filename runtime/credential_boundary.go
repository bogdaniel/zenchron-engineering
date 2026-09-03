package runtime

// The credential confidentiality boundary.
//
// Five different questions used to be answered by one function, and answering
// them together is what made the runtime unable to repair its own sandbox:
//
//	PATH SAFETY          is this path allowed to be addressed at all?
//	VALUE DETECTION      does this text contain a real credential VALUE?
//	PROVIDER ADMISSION   may a producer be shown this candidate at all?
//	OUTPUT REDACTION     what may a tool result carry back to the model?
//	COMMIT GATE          may this change become a runtime-owned commit?
//
// They are separate concerns with separate answers, so they are separate code.
// GuardCandidate keeps PATH SAFETY (adapters.go). Everything about credential
// VALUES lives here.
//
// The defect this replaces: a content predicate equivalent to
// contains("github_pat_") classified the source of a secret SCANNER as a
// secret. runtime/sandbox.go defines the transcript redaction regex, so the
// broker refused to read, search, or patch the one file the sandbox repair
// needed - while candidate.run mounted the same workspace and printed the file
// with sed. A refusal one capability can walk around is not a boundary; it is
// only an obstacle to the honest caller.
//
// The rule now: a bare token PREFIX or an environment-variable NAME is
// vocabulary and stays readable. A prefix followed by a plausible token BODY is
// a value, and a value is refused at admission, redacted in every model-visible
// tool result, and refused again at the commit gate.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// credentialRedaction is what replaces a detected value in model-visible text.
const credentialRedaction = "[REDACTED]"

// The value detectors. Each is deliberately high-confidence and deterministic:
// a known shape, plus enough body to be a real credential rather than the name
// of one. No entropy heuristics, no classifier, no network - a detector whose
// answer can change between two runs of the same bytes cannot gate a commit.
//
// Each pattern is written so that a Go source file DEFINING it does not match
// it. That is not a coincidence to be preserved by luck: it is asserted by
// TestTheCredentialDetectorDoesNotFlagItsOwnSource.
var (
	// githubTokenValue covers both live GitHub formats. A fine-grained token is
	// github_pat_ plus 22 identifier characters, a separator, and a long
	// secret; a classic token is ghp_ plus 36.
	//
	// The fine-grained half requires the SEPARATOR, not merely a long run of
	// identifier characters. Without it, github_pat_REPLACE_WITH_YOUR_TOKEN -
	// a placeholder in documentation - is a credential, which is the same
	// false positive this whole boundary exists to remove, one layer down.
	//
	// The tail bounds are minimums rather than exact lengths on purpose: an
	// exact-length rule silently stops detecting the day a format grows, and a
	// missed credential is worse than a slightly wider net that no prose fits
	// through anyway.
	githubTokenValue = regexp.MustCompile(`\b(?:github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{30,}|ghp_[A-Za-z0-9]{36,})`)
	// awsSecretValue requires the identifier to be ASSIGNED something. An AWS
	// secret access key is exactly 40 base64-alphabet characters, so the
	// identifier appearing in a list, a comment, or a struct tag is not a
	// finding; the identifier followed by = and 40 characters is.
	awsSecretValue = regexp.MustCompile(`((?i:aws_secret_access_key)["'\s]*[:=][ \t"']*)([A-Za-z0-9/+=]{40,})`)
	// pemPrivateKeyValue requires the whole envelope: a BEGIN marker, an
	// encoded body, and the matching END marker. A lone marker - which is what
	// a PEM parser's source, a redaction test, or a truncated fixture contains -
	// is not a private key.
	pemPrivateKeyValue = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----[\r\n]+[A-Za-z0-9+/=\r\n]{64,}?-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----`)
)

// ContainsCredentialValue reports whether text carries a high-confidence
// credential VALUE. It is the single answer to that question: admission, tool
// output and the commit gate all ask it, so they cannot disagree.
func ContainsCredentialValue(data []byte) bool {
	return githubTokenValue.Match(data) || awsSecretValue.Match(data) || pemPrivateKeyValue.Match(data)
}

// RedactCredentialValues replaces credential values with [REDACTED] and leaves
// everything else exactly as it was.
//
// The AWS case keeps its identifier and redacts only what was assigned to it,
// because "aws_secret_access_key = [REDACTED]" tells a model what it is looking
// at and "[REDACTED]" alone does not. Redaction exists to remove the secret,
// not to remove the model's ability to work.
func RedactCredentialValues(text string) string {
	text = githubTokenValue.ReplaceAllString(text, credentialRedaction)
	text = awsSecretValue.ReplaceAllString(text, "${1}"+credentialRedaction)
	return pemPrivateKeyValue.ReplaceAllString(text, credentialRedaction)
}

// sensitiveCredentialFilename reports whether a base name is a credential FILE
// by shape - a dotenv file, an SSH private key, a key blob, a credential store.
//
// The predicate it replaces was substring matching on "secret", "private" and
// "credential", which made secret_scanner.go, private_key_parser.go and
// credential_policy.go permanently unreachable. An engineering system that
// cannot open its own security code cannot maintain it.
func sensitiveCredentialFilename(base string) bool {
	lower := strings.ToLower(base)
	switch lower {
	case ".env", ".netrc", "_netrc", ".pgpass", ".htpasswd", "credentials",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ed25519_sk", "id_ecdsa_sk":
		return true
	}
	// .env.example and its siblings are committed documentation in a very large
	// share of repositories. Treating them as credentials would make those
	// repositories permanently unworkable, which is the availability half of
	// the same defect.
	switch lower {
	case ".env.example", ".env.sample", ".env.template", ".env.dist", ".env.defaults":
		return false
	}
	// deploy.env and production.env are the same dotenv shape as .env.local.
	if strings.HasPrefix(lower, ".env.") || strings.HasSuffix(lower, ".env") {
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".ppk"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// CredentialMaterialError is the typed local refusal. It names the path, never
// the value: a diagnostic that quotes the secret to explain the secret is the
// same leak by another route.
type CredentialMaterialError struct {
	// Path is workspace-relative, or "" when the scan could not decide.
	Path string
	// Kind is which of the three findings this is.
	Kind CredentialMaterialKind
	// Detail says what was wrong when the scan could not decide - an
	// unreadable entry, or content too large to scan deterministically.
	Detail string
}

// The three things candidate-visible content can be wrong about.
type CredentialMaterialKind string

const (
	// CredentialMaterialValue is a detected credential value.
	CredentialMaterialValue CredentialMaterialKind = "value"
	// CredentialMaterialFile is a credential FILE by shape. Its contents do
	// not have to look like anything: an opaque TOKEN=... in a .env is still a
	// credential, and a producer sandbox reads it whatever it contains.
	CredentialMaterialFile CredentialMaterialKind = "file"
	// CredentialMaterialInconclusive is the scan being unable to decide, which
	// is refused rather than assumed safe.
	CredentialMaterialInconclusive CredentialMaterialKind = "inconclusive"
)

func (e *CredentialMaterialError) Error() string {
	switch e.Kind {
	case CredentialMaterialFile:
		return fmt.Sprintf("candidate path %q is a credential file", e.Path)
	case CredentialMaterialInconclusive:
		if e.Path == "" {
			return "candidate credential scan is inconclusive: " + e.Detail
		}
		return fmt.Sprintf("candidate credential scan is inconclusive at %q: %s", e.Path, e.Detail)
	default:
		return fmt.Sprintf("candidate path %q contains a credential value", e.Path)
	}
}

// credentialScanFileLimit bounds one file the scan will read. Above it the scan
// says INCONCLUSIVE rather than assuming the file is safe: an admission gate
// that silently skips what it cannot read is not a gate. No source tree this
// runtime governs has a file this size; one that does is worth an operator
// looking at it.
const credentialScanFileLimit = 16 << 20

// ScanCandidateForCredentialValues walks everything a producer sandbox would be
// able to read and refuses on the first high-confidence credential value.
//
// It walks the workspace rather than asking Git, because the producer's mount
// shows the working tree: tracked files, untracked files and anything a
// previous invocation left behind are all equally visible to candidate.run.
// Runtime-owned Git metadata is excluded - it is masked inside the sandbox and
// is not candidate content.
//
// Symlinks are not followed and are not read: the workspace boundary is proven
// by ToolBroker.resolve and CandidateWorkspace, not re-litigated here.
func ScanCandidateForCredentialValues(root string) error {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return &CredentialMaterialError{Kind: CredentialMaterialInconclusive, Detail: "candidate workspace unavailable"}
	}
	return filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
		rel, relErr := filepath.Rel(resolved, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "workspace entry is unreadable"}
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "workspace entry is unreadable"}
		}
		// A symlink's target is either inside the workspace - and then it is
		// walked on its own - or outside it, where it is not candidate content.
		if !info.Mode().IsRegular() {
			return nil
		}
		// A credential FILE is refused on its name alone. Value detection
		// cannot help here: TOKEN=hunter2 in a .env is opaque, and the path
		// gate that refuses to BROKER .env does not stop candidate.run, which
		// has the whole workspace bind-mounted and can simply cat it. Admission
		// is the only layer where refusing it means anything.
		if sensitiveCredentialFilename(entry.Name()) {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialFile}
		}
		if info.Size() > credentialScanFileLimit {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "file exceeds the deterministic scan ceiling"}
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "workspace entry is unreadable"}
		}
		if ContainsCredentialValue(data) {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialValue}
		}
		return nil
	})
}

// scanPathsForCredentialValues is the commit-gate half: the same question asked
// of exactly the paths a commit is about to capture.
//
// Admission proves what went IN. This proves what came OUT, and the two are
// different facts: a producer can write a credential into a workspace that was
// clean when it was handed over. Neither one implies the other, so neither one
// is skipped.
func scanPathsForCredentialValues(root string, paths []string) error {
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			// A deleted path has no content to inspect. Deletion is not how a
			// credential enters a tree.
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > credentialScanFileLimit {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "file exceeds the deterministic scan ceiling"}
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialInconclusive, Detail: "changed path is unreadable"}
		}
		if ContainsCredentialValue(data) {
			return &CredentialMaterialError{Path: rel, Kind: CredentialMaterialValue}
		}
	}
	return nil
}
