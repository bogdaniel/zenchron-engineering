package runtime

import (
	"bufio"
	"errors"
	"testing"
)

// A request that was written and got no reply is ErrControlReplyLost - the
// supervisor may have applied it - while a request that never connected is
// not (#398). Mutation: drop the wrap after the write, and a lost reply reads
// as "nothing was sent".
func TestSendControlSeparatesALostReplyFromNothingSent(t *testing.T) {
	stateDir := controlStateDir(t)
	if _, err := SendControl(stateDir, ControlRequest{Command: ControlPing}); err == nil || errors.Is(err, ErrControlReplyLost) {
		t.Fatalf("no endpoint: %v, want a not-sent error", err)
	}
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.listener.Accept()
		if err != nil {
			return
		}
		_, _ = bufio.NewReader(connection).ReadBytes('\n')
		_ = connection.Close() // applied, perhaps; answered, never
	}()
	if _, err := SendControl(stateDir, ControlRequest{Command: ControlStop, RunID: "r"}); !errors.Is(err, ErrControlReplyLost) {
		t.Fatalf("sent and unanswered: %v, want ErrControlReplyLost", err)
	}
}
