package benchmark

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var caseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,80}$`)

// Emission is one arm's immutable evidence set: raw records, the derived
// result and the human-readable report.
type Emission struct {
	Result Result
	// Paths are the files written, in order: activity log copy, runtime
	// collection (when present), result, report.
	Paths []string
}

// Emit writes one arm's evidence set under outDir/<arm>/. Every file is created
// exclusively and read-only: an existing result is never overwritten, and a set
// that cannot be written completely leaves nothing behind. An invalid run is
// emitted with its reasons, not dropped.
//
// rawLog is the activity log's bytes exactly as read, so the digest recorded in
// the result is the digest of the copy preserved beside it.
func Emit(outDir string, in Input, rawLog []byte) (Emission, error) {
	if !caseIDPattern.MatchString(in.Case.ID) {
		return Emission{}, fmt.Errorf("case id %q must match %s", in.Case.ID, caseIDPattern)
	}
	if in.Arm != ArmPlanned && in.Arm != ArmDirect {
		return Emission{}, fmt.Errorf("unknown arm %q", in.Arm)
	}
	records, err := ParseRecords(bytes.NewReader(rawLog))
	if err == nil {
		in.Ledger, err = Fold(records)
	}
	in.LedgerErr = err
	base := in.RecordedAt.UTC().Format("2006-01-02") + "-" + in.Case.ID
	dir := filepath.Join(outDir, string(in.Arm))

	files := []file{{name: base + ".activity.jsonl", body: rawLog}}
	in.Sources = []SourceRef{{Kind: "activity_log", Path: files[0].name, SHA256: sha(rawLog)}}
	if in.Runtime != nil {
		body, err := json.MarshalIndent(in.Runtime, "", "  ")
		if err != nil {
			return Emission{}, fmt.Errorf("encode runtime collection: %w", err)
		}
		files = append(files, file{name: base + ".runtime.json", body: append(body, '\n')})
		in.Sources = append(in.Sources, SourceRef{Kind: "runtime_collection", Path: files[1].name, SHA256: sha(files[1].body)})
	}
	result, err := Build(in)
	if err != nil {
		return Emission{}, err
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return Emission{}, fmt.Errorf("encode result: %w", err)
	}
	files = append(files, file{name: base + ".json", body: append(body, '\n')},
		file{name: base + ".md", body: []byte(Report(result))})
	paths, err := writeExclusive(dir, files)
	if err != nil {
		return Emission{}, err
	}
	return Emission{Result: result, Paths: paths}, nil
}

type file struct {
	name string
	body []byte
}

func writeExclusive(dir string, files []file) ([]string, error) {
	for _, f := range files {
		if _, err := os.Lstat(filepath.Join(dir, f.name)); err == nil {
			return nil, fmt.Errorf("refusing to overwrite %s: results are immutable", filepath.Join(dir, f.name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := writeNew(path, f.body); err != nil {
			for _, w := range written {
				os.Remove(w)
			}
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

func writeNew(path string, body []byte) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := out.Write(body); err != nil {
		out.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return out.Close()
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Report renders the concise human-readable summary. It states validity and
// unknowns first, because a number without them is not evidence.
func Report(r Result) string {
	var b strings.Builder
	title := "Benchmark result: " + r.Case.ID + " (" + string(r.Mode) + ")"
	fmt.Fprintf(&b, "# %s\n\n", title)
	if r.Synthetic {
		b.WriteString("**SYNTHETIC EXAMPLE. Not evidence of anything.**\n\n")
	}
	if r.Validity.Valid {
		b.WriteString("Validity: valid.\n\n")
	} else {
		b.WriteString("Validity: **INVALID**. This run must not be used as evidence.\n\n")
		for _, reason := range r.Validity.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Outcome: %s (%s). Base %s. Instrumentation %s. Protocol %s.\n\n",
		r.Outcome.Result, r.Outcome.Source, r.Instrumentation.BaseSHA, r.Instrumentation.Version, r.Instrumentation.ProtocolDigest)
	b.WriteString("## Human attention\n\n")
	if s := r.Supervision; s != nil {
		fmt.Fprintf(&b, "- Operator: %s\n- Active human time: %.1f min (unattended waiting excluded: %.1f min)\n",
			s.Operator, s.ActiveHumanSeconds/60, s.UnattendedWaitSeconds/60)
		fmt.Fprintf(&b, "- Authority interventions: %d; final acceptance/merge steps: %d\n", s.AuthorityInterventions, s.FinalAcceptanceMerges)
		fmt.Fprintf(&b, "- Recovery workarounds: %d; manual relays: %d (%s: %d audited, %d self-reported)\n",
			s.RecoveryWorkarounds, s.Relays.Total, s.Relays.Basis, s.Relays.Audited, s.Relays.SelfReported)
	} else {
		b.WriteString("- unknown: no activity log entries for this arm\n")
	}
	b.WriteString("\n## Runtime\n\n")
	m := r.Measured
	fmt.Fprintf(&b, "- Child runs: %s\n- Operation attempts: %s\n- Review observations: %s; review continuations: %s\n",
		show(m.ChildRuns), show(m.OperationAttempts), show(m.ReviewCycles), show(m.RemediationCycles))
	fmt.Fprintf(&b, "- Peak run concurrency: %s; run overlap seconds: %s\n- Runtime wall seconds: %s\n",
		show(m.PeakRunConcurrency), show(m.RunOverlapSeconds), show(m.RuntimeWallSeconds))
	fmt.Fprintf(&b, "- Tokens: %s\n- Cost: %s\n", show(m.Tokens), showCost(m.Cost))
	b.WriteString("\n## Sources\n\n")
	for _, s := range r.Sources {
		fmt.Fprintf(&b, "- %s: `%s` sha256 `%s`\n", s.Kind, s.Path, s.SHA256)
	}
	fmt.Fprintf(&b, "\nContent digest: `%s`\n\n## Limitations\n\n", r.ContentDigest)
	for _, l := range r.Interpretation.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	return b.String()
}

func show[T any](m Measure[T]) string {
	if m.Value == nil {
		return "unknown (" + m.UnknownReason + ")"
	}
	return fmt.Sprint(*m.Value)
}

func showCost(c Cost) string {
	if !c.Known || c.AmountMicros == nil {
		return "unknown (" + c.Reason + ")"
	}
	return fmt.Sprintf("%d micros", *c.AmountMicros)
}
