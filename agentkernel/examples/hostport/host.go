package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// hostRunner is the host's process boundary. This host answers its one
// granted command in process; another host would spawn, contain and reap a
// real process here. The kernel never does.
type hostRunner struct {
	documents map[string]string
}

func (r hostRunner) Run(ctx context.Context, req api.CommandRequest) (api.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return api.CommandResult{}, err
	}
	if len(req.Argv) != 1 || req.Argv[0] != "list-documents" {
		return api.CommandResult{ExitCode: 127, Stderr: []byte("unknown command")}, nil
	}
	names := make([]string, 0, len(r.documents))
	for name := range r.documents {
		names = append(names, name)
	}
	sort.Strings(names)
	return api.CommandResult{Stdout: []byte(strings.Join(names, "\n"))}, nil
}

// jsonLines is the host's event sink: one JSON object per line. A write
// error is returned, so the kernel stops and settles recording_failed.
type jsonLines struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *jsonLines) Record(_ context.Context, ev api.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.w).Encode(ev)
}

// vault is the host's credential source. The request names only a handle.
type vault map[string]string

func (v vault) Credential(_ context.Context, handle string) (api.Secret, error) {
	value, ok := v[handle]
	if !ok {
		return api.Secret{}, fmt.Errorf("no credential for handle %q", handle)
	}
	return api.NewSecret(value), nil
}

// modelServer stands in for a local model server speaking the kernel's
// neutral protocol (providers/local). It is reached through the adapter's
// Doer port, so no network call is made; it checks the bearer credential the
// adapter resolved from the vault, then answers from a script.
type modelServer struct {
	token  string
	script *scripted.Provider
}

func (m modelServer) Do(r *http.Request) (*http.Response, error) {
	defer r.Body.Close()
	if r.Header.Get("Authorization") != "Bearer "+m.token {
		return reply(http.StatusUnauthorized, []byte(`{"error":{"message":"bad credential"}}`)), nil
	}
	var req api.ProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return reply(http.StatusBadRequest, []byte(`{"error":{"message":"bad request"}}`)), nil
	}
	resp, err := m.script.Complete(r.Context(), req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return reply(http.StatusOK, body), nil
}

func reply(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)),
	}
}
