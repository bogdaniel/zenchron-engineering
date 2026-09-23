package main

// THE TWO HALVES OF A LIVE TRANSITION, as this process performs them.
//
// runtime/controller_launch.go owns the order and the point of no return. This
// file owns only the transport: how a predecessor starts its successor, and how
// a successor started that way waits, reports, and proves it is serving.
//
// THE HANDSHAKE IS NOT AUTHORITY. Nothing a process says over this pipe grants
// it anything: the successor still acquires the role, still revalidates under
// exclusive ownership, and still proves its own generation against the durable
// record. The pipe answers one question the durable record cannot - "has the
// predecessor let go yet" - and a successor that acted on it without the
// checks below would be taking a process's word for who is in charge.
//
// IT IS A DEDICATED PAIR OF PIPES rather than stdio. The successor's stdout is
// the operator's serve output, with a banner and a JSON report per pass, and a
// protocol multiplexed onto that would be a parser waiting to misread a log
// line. Descriptors 3 and 4 carry the handshake and nothing else.
//
// THE SUCCESSOR OUTLIVES THE PREDECESSOR, which is the one asymmetry worth
// naming. It is started in its own process group so a signal aimed at the
// predecessor's terminal does not reach the controller that replaced it, and
// once it is serving it stops reading the pipe: the predecessor exits, the
// descriptor closes, and nothing about that is an event for a process that is
// now the controller.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// The handshake vocabulary. One line each, because a length-prefixed framing
// would be ceremony for three messages.
const (
	successorIdentityLine = "identity" // successor -> predecessor: what I am
	successorEvaluateLine = "evaluate" // predecessor -> successor: decide this transition
	successorDecidedLine  = "decided"  // successor -> predecessor: here is my decision
	successorProceedLine  = "proceed"  // predecessor -> successor: the role is free
	successorActiveLine   = "active"   // successor -> predecessor: I am serving
	successorErrorLine    = "error"    // successor -> predecessor: and why not
)

// Handshake bounds. They are generous and finite: a step that never answers
// must eventually be a refusal, because the alternative is a predecessor that
// waits forever holding a role it has already decided to give up.
const (
	// identifyTimeout covers process start, configuration load and self
	// measurement.
	identifyTimeout = 2 * time.Minute
	// activateTimeout covers acquisition, revalidation - which replays every
	// live run's journal - activation and opening service.
	activateTimeout = 15 * time.Minute
	// evaluateTimeout covers the successor replaying every live run's journal
	// under its own code, which is the same work revalidation does later.
	evaluateTimeout = 15 * time.Minute
	// quiesceTimeout bounds how long intake stays suspended waiting for the
	// work this controller started. A provider invocation of tens of minutes
	// is ordinary; a fleet that does not settle within this gives intake back
	// and the next pass tries again.
	quiesceTimeout = 30 * time.Minute
)

// The descriptors the successor reads and writes the handshake on. 0, 1 and 2
// stay exactly what they are for any other process.
const (
	successorControlFD = 3 // predecessor -> successor
	successorReportFD  = 4 // successor -> predecessor
)

// ---------------------------------------------------------------------------
// The predecessor's half: starting the successor
// ---------------------------------------------------------------------------

// spawnedSuccessor is a real successor process, inert until told otherwise.
type spawnedSuccessor struct {
	command *exec.Cmd
	control io.WriteCloser
	report  *bufio.Reader
	closers []io.Closer
}

// spawnInertSuccessor starts the artifact the transition names.
//
// IT IS STARTED WITH THIS PROCESS'S OWN ARGUMENTS, plus the transition. That is
// not a shortcut: the successor must resolve the SAME operator configuration,
// because succession requires the effective configuration digest to be
// unchanged, and reconstructing an equivalent command line is how two processes
// come to disagree about which config file they read.
func spawnInertSuccessor(artifact, handoffID string, args []string) (runtime.InertSuccessor, error) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		controlRead.Close()
		controlWrite.Close()
		return nil, err
	}
	command := exec.Command(artifact, append(append([]string{}, args...), "--successor-of", handoffID)...)
	command.ExtraFiles = []*os.File{controlRead, reportWrite} // becomes fd 3 and fd 4
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	// A NEW PROCESS GROUP, where the platform has them. The successor is about
	// to outlive the process starting it, and a Ctrl-C meant for the
	// predecessor's terminal must not reach a controller mid-activation.
	configureSuccessorProcess(command)
	if err := command.Start(); err != nil {
		for _, file := range []*os.File{controlRead, controlWrite, reportRead, reportWrite} {
			file.Close()
		}
		return nil, err
	}
	// The child holds its own copies; this process must not keep the write end
	// of the report pipe open, or a dead child would never look like EOF.
	controlRead.Close()
	reportWrite.Close()
	return &spawnedSuccessor{
		command: command, control: controlWrite, report: bufio.NewReader(reportRead),
		closers: []io.Closer{controlWrite, reportRead},
	}, nil
}

// Identify reads the generation the successor says it is.
func (s *spawnedSuccessor) Identify() (runtime.ControllerBinding, error) {
	line, err := s.await(successorIdentityLine, identifyTimeout)
	if err != nil {
		return runtime.ControllerBinding{}, err
	}
	var binding runtime.ControllerBinding
	if err := json.Unmarshal([]byte(line), &binding); err != nil {
		return runtime.ControllerBinding{}, fmt.Errorf("the successor's identity could not be read: %w", err)
	}
	return binding, nil
}

// Evaluate asks the successor to decide the transition against the state as it
// stands now, and returns the record carrying its decisions.
func (s *spawnedSuccessor) Evaluate(prepared runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
	var decided runtime.ControllerHandoff
	asked, err := json.Marshal(prepared)
	if err != nil {
		return decided, err
	}
	if _, err := io.WriteString(s.control, successorEvaluateLine+" "+string(asked)+"\n"); err != nil {
		return decided, err
	}
	line, err := s.await(successorDecidedLine, evaluateTimeout)
	if err != nil {
		return decided, err
	}
	if err := json.Unmarshal([]byte(line), &decided); err != nil {
		return decided, fmt.Errorf("the successor's decision could not be read: %w", err)
	}
	return decided, nil
}

// Proceed tells the successor the role has been released.
func (s *spawnedSuccessor) Proceed(handoffID string) error {
	_, err := io.WriteString(s.control, successorProceedLine+" "+handoffID+"\n")
	return err
}

// AwaitActive waits for the successor to prove it is serving.
func (s *spawnedSuccessor) AwaitActive() error {
	_, err := s.await(successorActiveLine, activateTimeout)
	return err
}

// Abandon stops a successor that will not be used.
//
// It is only ever called before the point of no return, when the successor
// holds no role, has written nothing and cannot be serving. Killing the process
// group rather than the process removes anything it started.
func (s *spawnedSuccessor) Abandon() error {
	defer s.close()
	return killSuccessorProcessTree(s.command)
}

// await reads one reply and requires it to be the expected one.
//
// A REFUSAL AND A SILENCE ARE DIFFERENT ANSWERS, and both are answers. The
// successor reports its own failures as `error <reason>`, which is how a
// predecessor learns WHY rather than that something did not happen.
func (s *spawnedSuccessor) await(expected string, within time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		line, err := s.report.ReadString('\n')
		lines <- result{strings.TrimSpace(line), err}
	}()
	select {
	case got := <-lines:
		if got.err != nil && got.line == "" {
			return "", fmt.Errorf("the successor stopped answering before it reported %q: %w", expected, got.err)
		}
		verb, rest, _ := strings.Cut(got.line, " ")
		switch verb {
		case expected:
			return rest, nil
		case successorErrorLine:
			return "", fmt.Errorf("the successor refused: %s", rest)
		default:
			return "", fmt.Errorf("the successor answered %q where %q was expected", got.line, expected)
		}
	case <-time.After(within):
		return "", fmt.Errorf("the successor did not report %q within %s", expected, within)
	}
}

func (s *spawnedSuccessor) close() {
	for _, closer := range s.closers {
		_ = closer.Close()
	}
}

// ---------------------------------------------------------------------------
// The successor's half: waiting, then proving
// ---------------------------------------------------------------------------

// successorHandshake is this process's end of the pipes, when it was started as
// somebody's successor.
type successorHandshake struct {
	handoffID string
	control   *bufio.Reader
	report    io.WriteCloser
}

// errNotStartedAsSuccessor is what `serve --successor-of` by hand looks like.
var errNotStartedAsSuccessor = fmt.Errorf(
	"--successor-of is the handshake a predecessor starts its successor through, and this process was not started that way")

// openSuccessorHandshake fails when the descriptors are not there, which is
// what running `serve --successor-of` by hand looks like. Refusing is right: a
// successor with nobody to hand over to would wait forever, or worse, decide
// on its own that it may proceed.
func openSuccessorHandshake(handoffID string) (*successorHandshake, error) {
	control := os.NewFile(successorControlFD, "successor-control")
	report := os.NewFile(successorReportFD, "successor-report")
	// os.NewFile wraps a descriptor without checking it, so the descriptors
	// are STATTED rather than assumed: an unopened fd 3 produces a File whose
	// every operation fails, and the honest place to discover that is here.
	for _, file := range []*os.File{control, report} {
		if file == nil {
			return nil, errNotStartedAsSuccessor
		}
		if _, err := file.Stat(); err != nil {
			return nil, errNotStartedAsSuccessor
		}
	}
	return &successorHandshake{handoffID: handoffID, control: bufio.NewReader(control), report: report}, nil
}

// announce reports what this process is, measured rather than declared.
func (h *successorHandshake) announce(binding runtime.ControllerBinding) error {
	encoded, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return h.write(successorIdentityLine + " " + string(encoded))
}

// serveUntilProceed answers the predecessor until it says the role has been
// released.
//
// THE ONLY THING THIS PROCESS DOES BEFORE THAT IS READ. It decides the
// transition - which is a read-only classification of the durable state, under
// this build's own decoders and replay - and it waits. It takes no role, opens
// no service and writes nothing, so a predecessor that abandons the attempt
// here abandons a process that holds nothing.
func (h *successorHandshake) serveUntilProceed(decide func(runtime.ControllerHandoff) (runtime.ControllerHandoff, error)) error {
	for {
		line, err := h.control.ReadString('\n')
		if err != nil {
			return fmt.Errorf("the predecessor stopped before releasing the controller role: %w", err)
		}
		verb, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch verb {
		case successorProceedLine:
			// It must be the transition this process was started for. A
			// predecessor signalling a different one is not a predecessor this
			// process has anything to do with.
			if rest != h.handoffID {
				return fmt.Errorf("the predecessor released transition %s and this process is the successor for %s", rest, h.handoffID)
			}
			return nil
		case successorEvaluateLine:
			var asked runtime.ControllerHandoff
			if err := json.Unmarshal([]byte(rest), &asked); err != nil {
				h.fail(fmt.Errorf("the transition to decide could not be read: %w", err))
				continue
			}
			decided, err := decide(asked)
			if err != nil {
				h.fail(err)
				continue
			}
			encoded, err := json.Marshal(decided)
			if err != nil {
				h.fail(err)
				continue
			}
			if err := h.write(successorDecidedLine + " " + string(encoded)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the predecessor said %q, which is not part of this handshake", strings.TrimSpace(line))
		}
	}
}

// active reports that this process is the activated generation and is serving.
func (h *successorHandshake) active() error { return h.write(successorActiveLine + " " + h.handoffID) }

// fail reports why this process is not serving, so the predecessor records a
// reason instead of a timeout.
func (h *successorHandshake) fail(cause error) { _ = h.write(successorErrorLine + " " + cause.Error()) }

func (h *successorHandshake) write(line string) error {
	_, err := io.WriteString(h.report, line+"\n")
	return err
}

// ---------------------------------------------------------------------------
// The successor's half, continued: becoming the controller
// ---------------------------------------------------------------------------

// controllerBinding is what this process would be recorded as: which program,
// which measured build, which effective configuration.
//
// It is the value a transition names its successor by, and this is the one
// place it is composed for a live process - the same three members, from the
// same sources, as the runtime binds a run to.
func (c *composition) controllerBinding() (runtime.ControllerBinding, error) {
	self, err := controllerSelf()
	if err != nil {
		return runtime.ControllerBinding{}, fmt.Errorf("this controller cannot establish its own identity: %w", err)
	}
	if self.Unattested {
		return runtime.ControllerBinding{}, fmt.Errorf("an unattested build has no adopted generation to succeed as")
	}
	build := self.Build
	return runtime.ControllerBinding{
		Controller: controllerIdentity(), Build: &build, Config: c.config.Digest,
	}, nil
}

// activateAsSuccessor completes the successor's half of the transition.
//
// The two calls are separate because the protocol separates them: activation
// establishes which generation is true, and opening service is a distinct,
// separately authorized step against the durable record. Neither is decided
// here - this reads the transition it was started for and asks.
func (c *composition) activateAsSuccessor(supervisor *runtime.Supervisor, role *runtime.ControllerRoleLease, handoffID string) error {
	self, err := controllerSelf()
	if err != nil {
		return fmt.Errorf("this controller cannot establish its own identity: %w", err)
	}
	service := runtime.BindControllerService(
		c.config.StateDir, controllerRoot(), c.store, self, role, supervisor)

	record, found, err := c.store.ControllerHandoff(handoffID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no transition %q is recorded, and a successor does not invent the transition it was started for", handoffID)
	}
	expect, err := runtime.Expect(record)
	if err != nil {
		return err
	}
	// NO ENDPOINT PROOF IS SUPPLIED, and the reason is that there is nothing
	// here for one to establish. That port exists for the case where the
	// process performing an activation is not the process at the endpoint; in
	// this path they are the same process by construction, and it has already
	// bound the socket - which it could only do because the predecessor had
	// given it up. A challenge-echo against its own loopback would prove that
	// this process is this process.
	if _, err := service.ActivateSuccessor(expect, nil); err != nil {
		// A projection that could not be repaired is reported and does not
		// stop service: the successor IS active and an operator's stable path
		// is stale. Every other error is a transition that did not happen.
		var drift *runtime.ProjectionRepairFailedError
		if !errors.As(err, &drift) {
			return err
		}
		fmt.Fprintf(os.Stderr, "the stable entrypoint needs repair: %v\n", err)
	}
	return service.EnableWorkAdmission(handoffID)
}

// ---------------------------------------------------------------------------
// The predecessor's half, continued: deciding there is a successor at all
// ---------------------------------------------------------------------------

// installControllerUpgrade wires the trusted-main updater and the launcher into
// the supervisor that will drive them.
//
// IT IS OFF FOR A CONTROLLER THAT CANNOT SUCCEED ITSELF, and says so rather
// than failing to start. An unattested build has no adopted lineage; a
// controller whose own generation is not published has no record naming the
// repository it came from; a missing governance credential means the trust root
// cannot be observed, and following trusted main without it would be following
// a branch under a gate nobody checked. None of those are reasons to refuse to
// serve - they are reasons not to upgrade - so each returns a sentence for the
// startup banner and leaves the supervisor unbound.
func (c *composition) installControllerUpgrade(supervisor *runtime.Supervisor, role *runtime.ControllerRoleLease, listener *runtime.ControlListener) (string, error) {
	self, err := controllerSelf()
	if err != nil {
		return "", fmt.Errorf("this controller cannot establish its own identity: %w", err)
	}
	if self.Unattested {
		return "off (this controller is an unattested build and has no adopted lineage to succeed)", nil
	}
	binding, err := c.controllerBinding()
	if err != nil {
		return "", err
	}
	// THE REPOSITORY COMES FROM THIS CONTROLLER'S OWN PROVENANCE, not from the
	// repositories it governs. A controller upgrades from the source it was
	// adopted from; the projects it works on are a different question, and
	// deriving one from the other would let an enrolled repository decide
	// which code becomes the next controller.
	running := runtime.RevisionRecord{Revision: self.Build.SourceRevision, Tree: self.Build.SourceTree}
	provenance, found, err := runtime.PublishedAdoptedController(controllerRoot(), running)
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf("off (this controller's generation %s is not published under %s, so its own provenance cannot be read)",
			self.Build.Version, controllerRoot()), nil
	}
	repository, err := runtime.ParseGitHubRepo(provenance.Repository)
	if err != nil {
		return "", err
	}
	governance, err := governanceObserver(c.config.GitHub)
	if err != nil {
		return "off (the governance credential that observes the trust root is not configured: " + err.Error() + ")", nil
	}
	remote, err := runtime.GovernedRemote(repository.CloneURL())
	if err != nil {
		return "", err
	}
	source, err := runtime.EnsureControllerSource(c.config.StateDir, remote, c.credentials)
	if err != nil {
		return "", err
	}
	deps := runtime.AdoptedBuildDeps{Governance: governance, RefSHA: c.forge.RefSHA}
	observe := func(ctx context.Context) (runtime.RevisionRecord, error) {
		return runtime.ObserveTrustedMainRevision(ctx, deps, repository, source)
	}
	service := runtime.BindControllerService(
		c.config.StateDir, controllerRoot(), c.store, self, role, supervisor)

	updater := runtime.NewControllerUpdater(binding, runtime.AdoptedBuildRequest{
		Repository:    repository,
		RepositoryDir: source,
		OutputRoot:    controllerRoot(),
		Sandbox: runtime.DockerSandbox{
			Image:    c.config.Assurance.Image,
			Endpoint: runtime.DockerEndpoint{Host: c.config.Assurance.DockerHost},
			StateDir: filepath.Join(c.config.StateDir, "artifacts", "docker-operations"),
		},
		DependencyCacheDir: c.config.Assurance.DependencyCacheDir,
	}, runtime.ControllerUpdaterPorts{
		ObserveTrustedMain: observe,
		Build: func(ctx context.Context, request runtime.AdoptedBuildRequest) (runtime.AdoptedBuildProvenance, error) {
			return runtime.BuildAdoptedController(ctx, request, deps, builderRecord())
		},
		Published: func(_ context.Context, subject runtime.RevisionRecord) (runtime.AdoptedBuildProvenance, bool, error) {
			return runtime.PublishedAdoptedController(controllerRoot(), subject)
		},
		Prepare: func(_ context.Context, successor runtime.ControllerBinding, artifact string) (runtime.ControllerHandoff, error) {
			return runtime.PrepareControllerSuccession(c.store, runtime.HandoffPreflightInput{
				Predecessor: runtime.HandoffParty{Binding: binding, ArtifactPath: self.ExecutablePath},
				Successor:   runtime.HandoffParty{Binding: successor, ArtifactPath: artifact},
				TrustedMain: runtime.RevisionRecord{
					Revision: successor.Build.SourceRevision, Tree: successor.Build.SourceTree},
				IsAncestor: runtime.LocalGitAncestry(source),
				Now:        time.Now().UTC(),
			})
		},
		Preflight: func(record runtime.ControllerHandoff) error {
			if !record.Compatible() {
				return fmt.Errorf("%s", strings.Join(record.Blockers(), "; "))
			}
			return nil
		},
	})

	launch := func(ctx context.Context, prepared runtime.ControllerHandoff, subject runtime.RevisionRecord) runtime.SuccessionLaunch {
		// The successor process is created by Spawn and has to be reachable
		// from Evaluate, which the launcher calls between two of its own
		// steps. It is one process per launch, and the launcher's order is
		// what guarantees the assignment happens before the use.
		var successor runtime.InertSuccessor
		return runtime.LaunchSuccession(ctx, prepared, subject, runtime.SuccessionPorts{
			Spawn: func(artifact, handoffID string) (runtime.InertSuccessor, error) {
				// THIS PROCESS'S OWN ARGUMENTS. The successor must resolve the
				// same operator configuration, because succession requires the
				// configuration digest to be unchanged.
				spawned, err := spawnInertSuccessor(artifact, handoffID, os.Args[1:])
				successor = spawned
				return spawned, err
			},
			ObserveTrustedMain: observe,
			Quiesce: func(ctx context.Context) (func(), error) {
				return supervisor.QuiesceWorkForTransition(ctx, quiesceTimeout)
			},
			Evaluate: func(_ context.Context, prepared runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
				asking, ok := successor.(*spawnedSuccessor)
				if !ok {
					return runtime.ControllerHandoff{}, fmt.Errorf("there is no successor process to decide the transition")
				}
				return asking.Evaluate(prepared)
			},
			Begin: func(prepared runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
				return service.BeginSuccession(prepared, listener.Close)
			},
		})
	}
	if err := supervisor.BindControllerUpgrade(runtime.NewControllerUpgrade(updater, launch)); err != nil {
		return "", err
	}
	return fmt.Sprintf("on (following trusted main of %s)", repository), nil
}

// builderRecord is this controller's truthful account of itself as a builder.
// A failed measurement is recorded as a failed measurement, never laundered
// into "unattested", which would claim a deliberate absence of provenance where
// there is a broken one.
func builderRecord() runtime.BuilderRecord {
	self, err := controllerBuild()
	if err != nil {
		return runtime.BuilderRecord{Kind: runtime.ControllerUnattested, ResolutionError: err.Error()}
	}
	record := runtime.BuilderRecord{Kind: self.Kind, Version: self.Version, SourceRevision: self.SourceRevision}
	if record.Kind == "" {
		record.Kind = runtime.ControllerUnattested
	}
	return record
}

// decideSuccession is the successor's own answer to whether it can continue
// every live run, asked of the durable state as it stands.
//
// IT IS READ-ONLY AND IT IS THIS BUILD'S ANSWER. The predecessor screens the
// same question earlier, from its own decoders, to avoid spending a container
// build on a successor that obviously cannot take over - but a predecessor
// cannot answer "can the successor replay this journal", and that is precisely
// the dimension a controller upgrade can fail on. So the decision that crosses
// the point of no return is made here, by the code that would do the reading,
// against a head the predecessor has already stopped moving.
func (c *composition) decideSuccession(asked runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
	self, err := controllerSelf()
	if err != nil {
		return runtime.ControllerHandoff{}, fmt.Errorf("this controller cannot establish its own identity: %w", err)
	}
	binding, err := c.controllerBinding()
	if err != nil {
		return runtime.ControllerHandoff{}, err
	}
	// THE SUCCESSOR IS THIS PROCESS, not whatever the request says it is. A
	// predecessor naming some other binding as the successor would be asking
	// this process to decide on behalf of a controller it is not.
	if err := self.ProvesGeneration(asked.Successor.Binding); err != nil {
		return runtime.ControllerHandoff{}, fmt.Errorf("this process is not the successor the transition names: %w", err)
	}
	stated, err := asked.Successor.Binding.Digest()
	if err != nil {
		return runtime.ControllerHandoff{}, err
	}
	mine, err := binding.Digest()
	if err != nil {
		return runtime.ControllerHandoff{}, err
	}
	if stated != mine {
		// The generations match and the bindings do not, which means the
		// identity or the effective configuration differs - exactly the
		// dimension succession requires to be unchanged, and exactly the one
		// the predecessor cannot check on this process's behalf.
		return runtime.ControllerHandoff{}, fmt.Errorf(
			"this process binds as %s and the transition names %s; the controller identity or the effective configuration differs",
			shortVersion(mine), shortVersion(stated))
	}
	return runtime.PrepareControllerSuccession(c.store, runtime.HandoffPreflightInput{
		Predecessor: asked.Predecessor,
		Successor:   runtime.HandoffParty{Binding: binding, ArtifactPath: asked.Successor.ArtifactPath},
		TrustedMain: runtime.RevisionRecord{
			Revision: binding.Build.SourceRevision, Tree: binding.Build.SourceTree},
		IsAncestor: runtime.LocalGitAncestry(runtime.ControllerSourceDir(c.config.StateDir)),
		Now:        time.Now().UTC(),
	})
}
