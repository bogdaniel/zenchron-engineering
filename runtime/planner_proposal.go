package runtime

import (
	"encoding/json"
	"fmt"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"strings"
)

// plannerProposal is the wire shape of the model's answer. It is decoded
// strictly and then translated into domain types, so an unknown member is a
// refusal rather than something silently carried into a plan.
type plannerProposal struct {
	Stages []plannerStage `json:"stages"`
	Notes  string         `json:"notes,omitempty"`
}

type plannerStage struct {
	ID                   string   `json:"id"`
	Kind                 string   `json:"kind"`
	Role                 string   `json:"role,omitempty"`
	ExecutionKind        string   `json:"execution_kind,omitempty"`
	Objective            string   `json:"objective,omitempty"`
	DependsOn            []string `json:"depends_on,omitempty"`
	RequiresCapabilities []string `json:"requires_capabilities,omitempty"`
	Independence         *struct {
		Dimension     string   `json:"dimension"`
		DifferentFrom []string `json:"different_from"`
	} `json:"independence,omitempty"`
	RequiredClaims []string `json:"required_claims,omitempty"`
	Rationale      string   `json:"rationale,omitempty"`
}

// decodePlannerProposal extracts and translates the answer.
//
// Everything it refuses, it refuses HERE rather than later: an unknown role, an
// unknown capability, an unknown stage kind or an unparseable answer are all
// conditions an operator can act on, and carrying them further would turn a bad
// answer into a bad plan.
func decodePlannerProposal(answer string, input PlannerInput) ([]domain.PlanStage, string, error) {
	body, err := extractJSONObject(answer)
	if err != nil {
		return nil, "", &PlannerRefusedError{AgentID: input.Agent.ID, Detail: err.Error()}
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	var proposal plannerProposal
	if err := decoder.Decode(&proposal); err != nil {
		return nil, "", &PlannerRefusedError{AgentID: input.Agent.ID, Detail: "the answer is not the stated JSON object: " + err.Error()}
	}
	if len(proposal.Stages) == 0 {
		return nil, "", &PlannerRefusedError{AgentID: input.Agent.ID, Detail: "the answer proposed no stages"}
	}
	stages := make([]domain.PlanStage, 0, len(proposal.Stages))
	for _, stage := range proposal.Stages {
		translated, err := translateStage(stage)
		if err != nil {
			return nil, "", &PlannerRefusedError{AgentID: input.Agent.ID, Detail: err.Error()}
		}
		stages = append(stages, translated)
	}
	return stages, boundedDetail(proposal.Notes), nil
}

func translateStage(stage plannerStage) (domain.PlanStage, error) {
	if strings.TrimSpace(stage.ID) == "" {
		return domain.PlanStage{}, fmt.Errorf("a proposed stage has no id")
	}
	kind := domain.StageKind(stage.Kind)
	algorithm := orchestration.WorkUnitExecutionKind(stage.ExecutionKind)
	if !orchestration.KnownExecutionKind(algorithm) {
		return domain.PlanStage{}, fmt.Errorf("proposed stage %q has an invalid execution_kind", stage.ID)
	}
	switch kind {
	case domain.StageAgent, domain.StageAssuranceGate, domain.StageHumanDecisionGate:
	default:
		return domain.PlanStage{}, fmt.Errorf("proposed stage %q has kind %q, which is not a stage kind", stage.ID, stage.Kind)
	}
	translated := domain.PlanStage{
		ID: stage.ID, Kind: kind, Objective: strings.TrimSpace(stage.Objective), ExecutionKind: stage.ExecutionKind,
		DependsOn: stage.DependsOn, RequiredClaims: stage.RequiredClaims,
		Rationale: boundedDetail(stage.Rationale),
	}
	if kind != domain.StageAgent {
		// A gate that states worker requirements is REFUSED, not cleaned up.
		// The translation below copies role and capabilities only for an agent
		// stage, so a gate carrying them lost them here and reached the graph
		// laws looking innocent - which is the same laundering the compiler
		// deliberately refuses to do: a planner that asked for a gate performed
		// by a worker got a gate, and nobody was told it had asked.
		if strings.TrimSpace(stage.Role) != "" {
			return domain.PlanStage{}, fmt.Errorf("proposed stage %q is a %s and names role %q: a gate references existing evidence or a human decision and is not performed by a worker", stage.ID, kind, stage.Role)
		}
		if len(stage.RequiresCapabilities) > 0 {
			return domain.PlanStage{}, fmt.Errorf("proposed stage %q is a %s and requires capabilities: a gate executes nothing", stage.ID, kind)
		}
	}
	if kind == domain.StageAgent {
		role := domain.EngineeringRole(stage.Role)
		if !domain.KnownRole(role) {
			return domain.PlanStage{}, fmt.Errorf("proposed stage %q names role %q, which is not in the role catalogue", stage.ID, stage.Role)
		}
		translated.Role = role
		for _, capability := range stage.RequiresCapabilities {
			typed := domain.EngineeringCapability(capability)
			if !domain.KnownCapability(typed) {
				return domain.PlanStage{}, fmt.Errorf("proposed stage %q requires capability %q, which is not in the v0 ontology", stage.ID, capability)
			}
			translated.RequiresCapabilities = append(translated.RequiresCapabilities, typed)
		}
	}
	if stage.Independence != nil {
		dimension := domain.IndependenceDimension(stage.Independence.Dimension)
		known := false
		for _, candidate := range domain.IndependenceDimensions() {
			if candidate == dimension {
				known = true
			}
		}
		if !known {
			return domain.PlanStage{}, fmt.Errorf("proposed stage %q requires independence dimension %q, which is not a dimension", stage.ID, stage.Independence.Dimension)
		}
		translated.Independence = &domain.IndependenceRequirement{
			Dimension: dimension, DifferentFrom: stage.Independence.DifferentFrom,
		}
	}
	return translated, nil
}

// extractJSONObject finds the model's answer inside its prose.
//
// A coding CLI prints its own progress around the answer, so the answer is
// located rather than assumed to be the whole output: the LAST balanced JSON
// object containing a "stages" member wins, because a model that restates its
// answer ends with the one it means.
//
// The FENCE is tried first, and it is not a convenience. The brace scan below
// treats a whole transcript as brace-and-quote structure, and a coding CLI's
// transcript is mostly not that: it echoes Go source, test names and prose, so
// unmatched braces and odd quotes accumulate and the scanner's idea of "inside a
// string" stops matching reality. On a real #119 planning transcript that left
// 29 unclosed braces and 16 recorded spans, the model's perfectly good answer
// never formed a span at all, and the last thing that did parse was the output
// contract's own example - which the provider had echoed as part of the prompt.
// The runtime then refused a stage called "kebab-case-id".
//
// A fence has none of that ambiguity: the contract asks for the answer in a
// fenced json block, the provider echoes the contract as plain text rather than
// as a fence, and the fence delimiters say exactly where the answer starts and
// stops. The brace scan stays as the fallback for a model that answers without
// one.
// It is a SINGLE pass over the transcript, keeping the position of every open
// brace on a stack. The earlier form restarted the scan at each unclosed brace,
// which is quadratic in the number of unclosed braces - and a coding CLI that
// echoes source code produces plenty of those, so an ordinary transcript could
// stall planning for minutes before the answer was even parsed.
func extractJSONObject(answer string) (string, error) {
	if candidate, found := lastFencedProposal(answer); found {
		return candidate, nil
	}
	// ONE pass records where each balanced span begins and ends. Nothing is
	// parsed here: recording a span costs the two indices, whatever the span
	// contains.
	type span struct{ start, end int }
	var spans []span
	var opens []int
	inString, escaped := false, false
	for i := 0; i < len(answer); i++ {
		character := answer[i]
		switch {
		case escaped:
			escaped = false
		case character == '\\' && inString:
			escaped = true
		case character == '"':
			inString = !inString
		case inString:
		case character == '{':
			opens = append(opens, i)
		case character == '}':
			if len(opens) == 0 {
				continue
			}
			start := opens[len(opens)-1]
			opens = opens[:len(opens)-1]
			spans = append(spans, span{start: start, end: i + 1})
			// A bound on how many spans are REMEMBERED, so a transcript of
			// nothing but braces cannot grow this slice without limit. The
			// answer ends the output, so the newest spans are the ones that
			// matter and the oldest are dropped.
			if len(spans) > maxPlannerCandidates {
				spans = spans[len(spans)-maxPlannerCandidates:]
			}
		}
	}

	// The LAST balanced object containing "stages" is the answer - a model that
	// restates its answer ends with the one it means - so the search runs
	// BACKWARDS and stops at the first candidate that parses. Validating
	// forwards charged the whole nested prefix of a noisy transcript before
	// reaching the answer, and could exhaust its own budget before it got
	// there: a stale earlier restatement then became the proposal, which is the
	// worst outcome available.
	budget := maxPlannerCandidateBytes
	for i := len(spans) - 1; i >= 0; i-- {
		candidate := answer[spans[i].start:spans[i].end]
		if len(candidate) > budget {
			// The budget bounds the work spent LOOKING. Reaching it means the
			// answer was not found in a bounded search rather than that one was
			// found: a refusal, never a stale substitute.
			break
		}
		budget -= len(candidate)
		if !strings.Contains(candidate, `"stages"`) {
			continue
		}
		// VALID JSON is not enough: `{"stages": 3}` is valid, contains the
		// member, and is not an answer. Decoding it here rather than at the
		// caller means trailing noise that happens to parse does not stop the
		// search at a candidate the strict decode would refuse.
		if json.Valid([]byte(candidate)) && looksLikeProposal(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no JSON object with a stages member was found in the answer")
}

// startsLine reports whether a fence marker begins its line, allowing the up to
// three spaces of indentation CommonMark permits.
func startsLine(text string, at int) bool {
	for indent := 0; indent <= 3; indent++ {
		i := at - indent
		if i == 0 {
			return true
		}
		if text[i-1] == '\n' {
			return true
		}
		if text[i-1] != ' ' {
			return false
		}
	}
	return false
}

// maxPlannerFences bounds how many fenced blocks are remembered. A transcript
// that is nothing but code fences cannot grow this without limit, and the
// answer ends the output, so the newest fences are the ones that matter.
const maxPlannerFences = 512

// lastFencedProposal is the last fenced block that parses as a proposal.
//
// Fences are paired in order - open, close, open, close - and searched from the
// END, because a model that restates its answer ends with the one it means. A
// final fence the provider never closed is still read: its content is the rest
// of the output, and an answer cut off mid-fence fails to parse here rather than
// being mistaken for something else.
//
// A fence must START A LINE, as CommonMark requires. Counting every occurrence
// of three backticks counted the ones a model writes INSIDE a sentence - "I
// will wrap the answer in ``` fences" - and one of those flips every subsequent
// open/close assignment: the real answer's opening fence becomes a close,
// nothing parses, and the whole thing falls back to the brace scan. That is the
// #119 failure returning, intermittently, decided by the model's prose.
//
// One consequence worth stating: a model that answers in a fence and then
// restates the same answer WITHOUT one gets the fenced version. The fence is
// the channel the contract asks for, and preferring it over later unfenced
// prose is the point rather than an accident.
func lastFencedProposal(answer string) (string, bool) {
	const fence = "```"
	var marks []int
	for offset := 0; ; {
		next := strings.Index(answer[offset:], fence)
		if next < 0 {
			break
		}
		at := offset + next
		offset = at + len(fence)
		if !startsLine(answer, at) {
			continue
		}
		marks = append(marks, at)
		// Dropped in PAIRS, so the open/close alternation the pairing below
		// depends on is preserved whatever is discarded.
		if len(marks) > maxPlannerFences {
			marks = marks[2:]
		}
	}
	type block struct{ open, end int }
	blocks := make([]block, 0, len(marks)/2+1)
	for i := 0; i < len(marks); i += 2 {
		end := len(answer)
		if i+1 < len(marks) {
			end = marks[i+1]
		}
		blocks = append(blocks, block{open: marks[i], end: end})
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		body := strings.TrimSpace(answer[blocks[i].open+len(fence) : blocks[i].end])
		// The optional language tag is the remainder of the fence's own line.
		// A first line carrying a brace is content rather than a tag.
		if newline := strings.IndexByte(body, '\n'); newline >= 0 && !strings.ContainsAny(body[:newline], "{}") {
			body = strings.TrimSpace(body[newline+1:])
		}
		if json.Valid([]byte(body)) && looksLikeProposal(body) {
			return body, true
		}
	}
	return "", false
}
