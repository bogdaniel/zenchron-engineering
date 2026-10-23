package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func controlPlane(args []string, stdout io.Writer) (int, error) {
	// A termination signal ends the dashboard cleanly, which is what lets
	// `down` stop it and its process record go with it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return controlPlaneUntil(ctx, args, stdout)
}

// controlPlaneUntil serves until ctx is cancelled, then shuts the server down
// and returns only after it has stopped, so a caller owns the listener's life.
func controlPlaneUntil(ctx context.Context, args []string, stdout io.Writer) (int, error) {
	flags := flag.NewFlagSet("control-plane", flag.ContinueOnError)
	flags.SetOutput(stdout)
	state := flags.String("state-dir", "", "existing Zenchron state directory")
	address := flags.String("listen", "127.0.0.1:8787", "loopback listen address")
	root := flags.String("controller-root", "", "adopted controller root (defaults to standard location)")
	auxiliary := flags.Bool("auxiliary", false, "start a read-only instance beside the primary on a different --listen address")
	if err := flags.Parse(args); err != nil {
		return 1, err
	}
	listenGiven := false
	flags.Visit(func(f *flag.Flag) { listenGiven = listenGiven || f.Name == "listen" })
	if flags.NArg() != 0 {
		return 1, fmt.Errorf("control-plane does not accept positional arguments")
	}
	if *root == "" {
		*root = controllerRoot()
	}
	store, err := runtime.OpenReadStore(*state)
	if err != nil {
		return 1, err
	}
	defer store.Close()
	if *auxiliary {
		if err := checkAuxiliary(*state, *address, listenGiven); err != nil {
			return 1, err
		}
	}
	token, err := controlplane.LoadToken(*state)
	if err != nil {
		return 1, err
	}
	if !*auxiliary {
		// The claim comes BEFORE anything is bound or recorded: a record read
		// first would let two dashboards starting together both pass.
		primary, err := runtime.AcquireControlPlanePrimary(*state)
		if errors.Is(err, runtime.ErrControlPlanePrimaryHeld) {
			return 1, primaryHeldError(*state)
		}
		if err != nil {
			return 1, err
		}
		defer primary.Release()
	}
	listener, err := controlplane.Listen(*address)
	if err != nil {
		return 1, err
	}
	defer listener.Close()
	// The address is not a secret; the token is, so only the file holding it is named.
	kind := "control plane"
	if *auxiliary {
		kind = "auxiliary read-only control plane"
	}
	fmt.Fprintf(stdout, "%s:http://%s/app (sign in with the token in %s)\n",
		kind, listener.Addr(), filepath.Join(*state, controlplane.TokenFile))
	observe := observeLiveController(*state)
	// Send is the ONLY mutation path: one governed request to the controller's
	// own socket. The store handle above stays read-only.
	send := func(request runtime.ControlRequest) (runtime.ControlResponse, error) {
		return runtime.SendControl(*state, request)
	}
	if *auxiliary {
		send = controlplane.RefuseSend
	}
	api := &controlplane.API{Store: store, Token: token, ControllerRoot: *root, Observe: observe, Send: send, Listen: listener.Addr().String()}
	web := &controlplane.Web{Store: store, Token: token, ControllerRoot: *root, Observe: observe}
	// One listener, one process, one token: the console is a second
	// presentation of exactly the boundary API owns, mounted beside it on
	// the same server rather than a second one (see controlplane.Web's own
	// doc comment).
	mux := http.NewServeMux()
	mux.Handle("/v1/", api.Handler())
	mux.Handle("/", web.Handler())
	handler := http.Handler(mux)
	if *auxiliary {
		// Server-side: nothing but reads gets through, whatever a client sends.
		handler = controlplane.ReadOnly(mux)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	// Only the primary is recorded: `up`, `down` and `status` find one
	// unambiguous dashboard, and an auxiliary instance can neither replace nor
	// delete its record.
	if !*auxiliary {
		if err := writeProcessRecord(*state, serviceControlPlane, selfProcessRecord(listener.Addr().String())); err != nil {
			return 1, err
		}
		defer clearProcessRecord(*state, serviceControlPlane)
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-stopped:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return 0, nil
	}
	return 1, err
}

// checkAuxiliary refuses an auxiliary instance that could collide with, or
// stand in for, the primary: it needs its own explicit address, and the token
// the primary created (an auxiliary instance writes nothing into the state
// directory).
func checkAuxiliary(state, address string, listenGiven bool) error {
	if !listenGiven {
		return errors.New("--auxiliary needs an explicit --listen address different from the primary's")
	}
	if record, ok, err := readProcessRecord(state, serviceControlPlane); err == nil && ok && record.Listen == address {
		return fmt.Errorf("--listen %s is the primary control plane's own address (pid %d); choose another", address, record.PID)
	}
	if _, err := os.Stat(filepath.Join(state, controlplane.TokenFile)); err != nil {
		return fmt.Errorf("--auxiliary needs the primary's token in %s; start the primary first", state)
	}
	return nil
}

// primaryHeldError names the live primary from its record, when it has written
// one, and the way out. The record is advice only; the lock is the fact.
func primaryHeldError(state string) error {
	where := "its address is not recorded yet"
	if record, ok, err := readProcessRecord(state, serviceControlPlane); err == nil && ok {
		where = fmt.Sprintf("pid %d on http://%s/", record.PID, record.Listen)
	}
	return fmt.Errorf("a primary control plane already runs for this state directory (%s); stop it with `zenchron-engineering down`, or add --auxiliary --listen <another address> for a read-only instance", where)
}
