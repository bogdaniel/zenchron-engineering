package main

// `control-plane`, the first production-shaped read boundary.
//
// It is a SEPARATE process from `serve`: nothing here acquires the controller
// role, drives a run, or opens a write-capable store. Every answer it gives
// comes from an existing runtime projection read through a connection that
// cannot mutate the database it serves (runtime.OpenSQLiteOperationStoreReadOnly).
// A frontend that wants to show fleet state talks to this instead of binding
// to runtime.db or to internal rows directly.
//
// Security is two defaults, not configuration an operator has to discover:
// loopback-only unless --addr names something else, and a bearer token on
// every request, generated once and persisted owner-only (0600) beside the
// runtime database it reads.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const controlPlaneUsage = "control-plane [--config <path>] [--addr host:port]"

// controlPlaneCommand starts the read-only HTTP control plane and serves
// until it is signalled, exactly like serveCommand does for the supervisor.
func controlPlaneCommand(args []string, stdout io.Writer) (int, error) {
	configPath := ""
	addr := "127.0.0.1:0"
	for len(args) > 0 {
		switch args[0] {
		case "--config":
			if len(args) < 2 {
				return runtime.ExitInvalid, fmt.Errorf("--config requires a path")
			}
			configPath, args = args[1], args[2:]
		case "--addr":
			if len(args) < 2 {
				return runtime.ExitInvalid, fmt.Errorf("--addr requires host:port")
			}
			addr, args = args[1], args[2:]
		default:
			return runtime.ExitInvalid, fmt.Errorf("control-plane does not accept %q; it takes %s", args[0], controlPlaneUsage)
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return runtime.ExitInvalid, fmt.Errorf("--addr must be host:port: %w", err)
	}
	if !isLoopbackHost(host) {
		return runtime.ExitInvalid, fmt.Errorf("--addr must bind a loopback address; %q is not loopback", host)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}
	cfg, err := runtime.LoadConfig(configPath, cwd)
	if err != nil {
		return 2, err
	}

	store, err := runtime.OpenSQLiteOperationStoreReadOnly(cfg.StateDir)
	if err != nil {
		return 2, err
	}
	defer store.Close()

	watch, err := cfg.WatchSettings()
	if err != nil {
		return 2, err
	}

	token, err := loadOrCreateControlPlaneToken(cfg.StateDir)
	if err != nil {
		return 2, err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return 2, err
	}

	server := newControlPlaneServer(store, cfg.StateDir, watch.MaxConcurrentRuns, token)
	httpServer := &http.Server{Handler: server.handler()}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	fmt.Fprintf(stdout, "zenchron-engineering control-plane\n")
	fmt.Fprintf(stdout, "  listening         %s\n", listener.Addr().String())
	fmt.Fprintf(stdout, "  state directory   %s\n", cfg.StateDir)
	fmt.Fprintf(stdout, "  token             %s\n", controlPlaneDir(cfg.StateDir)+"/token")

	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return 2, err
		}
		return runtime.ExitCompleted, nil
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return 2, err
		}
		return runtime.ExitCompleted, nil
	}
}

// isLoopbackHost reports whether host names only the local machine: an empty
// host (bind-all), a non-loopback literal address, or a hostname that is not
// "localhost" is refused, because any of those could make this listener
// reachable from off the machine - and the bearer token is this boundary's
// only defense once that is true.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
