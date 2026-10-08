package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestCancellationOf(t *testing.T) {
	bare, cancelBare := context.WithCancel(context.Background())
	cancelBare()

	stopped, stop := context.WithCancelCause(context.Background())
	stop(api.Cancellation(api.CancelOperatorStop))

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	cases := map[string]struct {
		ctx  context.Context
		want api.CancellationProvenance
	}{
		"bare cancel is unknown":         {bare, api.CancelUnknown},
		"host cause is preserved":        {stopped, api.CancelOperatorStop},
		"expired deadline":               {expired, api.CancelDeadline},
		"child keeps the parent's cause": {childOf(t, stopped), api.CancelOperatorStop},
		"live context has no provenance": {context.Background(), api.CancelUnknown},
	}
	for name, tc := range cases {
		if got := api.CancellationOf(tc.ctx); got != tc.want {
			t.Errorf("%s: CancellationOf = %q, want %q", name, got, tc.want)
		}
	}
}

func childOf(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	child, cancel := context.WithCancel(parent)
	t.Cleanup(cancel)
	return child
}
