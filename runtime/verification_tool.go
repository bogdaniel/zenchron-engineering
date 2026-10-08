package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// verificationToolGrant is runtime-owned attempt input, outside the candidate.
// The shim grants no executable beyond the contract's existing RequiredTools.
type verificationToolGrant struct {
	Parent          ExecutionAttemptRef `json:"parent"`
	StateDir        string              `json:"state_dir"`
	ControllerOwner string              `json:"controller_owner"`
	ScratchDir      string              `json:"scratch_dir"`
	RequestDir      string              `json:"request_dir"`
	Deadline        time.Time           `json:"deadline"`
	SearchPath      string              `json:"search_path"`
	Tools           map[string]string   `json:"tools"`
	PublicKey       []byte              `json:"public_key,omitempty"`
}

func prepareVerificationTools(v verificationExecution, tools, broker []string, searchPath, scratchDir string) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	if len(broker) == 0 || !filepath.IsAbs(broker[0]) {
		return "", errors.New("nested verification requires the controller's tool broker")
	}
	base, err := GitGuardDir(v.StateDir, v.Parent)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "verification")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		return "", err
	}
	if !filepath.IsAbs(scratchDir) {
		return "", errors.New("nested verification requires the existing attempt scratch grant")
	}
	op, _, found, err := v.Scheduler.Store.Operation(v.Parent.OperationID)
	if err != nil {
		return "", err
	}
	if !found || op.Deadline == nil {
		return "", errors.New("native tool parent lacks its physical deadline")
	}
	grant := verificationToolGrant{Parent: v.Parent, StateDir: v.StateDir, ControllerOwner: v.Scheduler.Owner,
		ScratchDir: scratchDir, RequestDir: filepath.Join(scratchDir, "verification-requests"), Deadline: *op.Deadline,
		SearchPath: searchPath, Tools: map[string]string{}}
	for _, tool := range tools {
		if tool == "" || tool == "." || tool == ".." || strings.ContainsAny(tool, "/\\\x00") {
			return "", fmt.Errorf("invalid contract tool executable %q", tool)
		}
		for _, entry := range filepath.SplitList(searchPath) {
			if !filepath.IsAbs(entry) {
				continue
			}
			path := filepath.Join(entry, tool)
			info, err := os.Stat(path)
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				grant.Tools[tool] = path
				break
			}
		}
		if grant.Tools[tool] == "" {
			return "", fmt.Errorf("contract tool %q unavailable on runtime PATH", tool)
		}
	}
	descriptor := filepath.Join(dir, "grant.json")
	data, err := CanonicalJSON(grant)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(descriptor, data, 0600); err != nil {
		return "", err
	}
	for tool := range grant.Tools {
		var argv []string
		for _, arg := range append(append([]string{}, broker...), descriptor, tool) {
			argv = append(argv, shellSingleQuoted(arg))
		}
		script := "#!/bin/sh\nexec " + strings.Join(argv, " ") + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0700); err != nil {
			return "", err
		}
	}
	return bin, nil
}

// RunVerificationTool is the hidden native broker entry point. The bounded
// process owns the grant, including descendants holding its ownership fd.
// The entire tool subtree consumes one permit; its PATH restores the original
// runtime tool path, avoiding recursive acquisition of the same resource.
func RunVerificationTool(ctx context.Context, descriptor, tool string, args []string) (CommandOutput, error) {
	var grant verificationToolGrant
	if err := readVerificationMessage(descriptor, &grant); err != nil {
		return CommandOutput{}, err
	}
	base, err := GitGuardDir(grant.StateDir, grant.Parent)
	if err != nil {
		return CommandOutput{}, err
	}
	if descriptor != filepath.Join(base, "verification", "grant.json") || grant.Deadline.IsZero() ||
		len(grant.PublicKey) != ed25519.PublicKeySize || !filepath.IsAbs(grant.ScratchDir) || grant.RequestDir != filepath.Join(grant.ScratchDir, "verification-requests") || !filepath.IsAbs(grant.Tools[tool]) {
		return CommandOutput{}, errors.New("invalid runtime verification tool grant")
	}
	ctx, cancel := context.WithDeadline(ctx, grant.Deadline)
	defer cancel()
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return CommandOutput{}, err
	}
	id := hex.EncodeToString(identity[:])
	request := verificationRequest{ID: id, Tool: tool, Owner: NewRuntimeOwner() + "-tool-" + id}
	// A dead provider/controller or a durable stop ends the tool. This is
	// observation of the same parent authority, not a second scheduler.
	stop := make(chan struct{})
	done := make(chan struct{})
	providerParent := os.Getppid()
	providerToken, _ := processStartToken(providerParent)
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				currentToken, tokenKnown := processStartToken(providerParent)
				if !processExists(providerParent) || (providerToken != "" && tokenKnown && providerToken != currentToken) {
					cancel()
					return
				}
				alive, decided := NewLockOwnerLiveness(grant.StateDir).observe(grant.ControllerOwner)
				if decided && !alive {
					cancel()
					return
				}
				reply, err := readVerificationReply(grant, request)
				if err != nil && !os.IsNotExist(err) || err == nil && (reply.Error != "" || reply.Permit.State == VerificationReleased) {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	lock, err := AcquireControllerInstanceLock(grant.ScratchDir, request.Owner)
	if err != nil {
		return CommandOutput{}, err
	}
	if err := writeVerificationMessage(filepath.Join(grant.RequestDir, id+".request.json"), request); err != nil {
		return CommandOutput{}, errors.Join(err, lock.Release())
	}
	if _, err := waitVerificationReply(ctx, grant, request, false); err != nil {
		return CommandOutput{}, errors.Join(err, finishNativeVerification(grant, request, lock))
	}
	env := os.Environ()
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + grant.SearchPath
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return CommandOutput{}, errors.Join(err, finishNativeVerification(grant, request, lock))
	}
	toolCtx := context.WithValue(ctx, verificationOwnerFileKey{}, lock.file)
	out, runErr := (OSCommandExecutor{}).Run(toolCtx, grant.Tools[tool], args, dir, env, time.Second)
	return out, errors.Join(runErr, finishNativeVerification(grant, request, lock))
}
