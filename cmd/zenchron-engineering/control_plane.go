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
	if err := flags.Parse(args); err != nil {
		return 1, err
	}
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
	token, err := controlplane.LoadToken(*state)
	if err != nil {
		return 1, err
	}
	listener, err := controlplane.Listen(*address)
	if err != nil {
		return 1, err
	}
	defer listener.Close()
	// The address is not a secret; the token is, so only the file holding it is named.
	fmt.Fprintf(stdout, "control plane:http://%s/app (sign in with the token in %s)\n",
		listener.Addr(), filepath.Join(*state, controlplane.TokenFile))
	observe := observeLiveController(*state)
	// Send is the ONLY mutation path: one governed request to the controller's
	// own socket. The store handle above stays read-only.
	send := func(request runtime.ControlRequest) (runtime.ControlResponse, error) {
		return runtime.SendControl(*state, request)
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
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	if err := writeProcessRecord(*state, serviceControlPlane, processRecord{PID: os.Getpid(), Listen: listener.Addr().String()}); err != nil {
		return 1, err
	}
	defer clearProcessRecord(*state, serviceControlPlane)
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
