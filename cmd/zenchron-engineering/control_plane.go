package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func controlPlane(args []string, stdout io.Writer) (int, error) {
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
	api := &controlplane.API{Store: store, Token: token, ControllerRoot: *root, Observe: observeLiveController(*state)}
	server := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	return 1, server.Serve(listener)
}
