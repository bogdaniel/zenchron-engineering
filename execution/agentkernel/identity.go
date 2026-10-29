package agentkernel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// The kernel identity of a host invocation (integration plan §5.11):
//
//	execution_id = "zx-" + hex(SHA-256(lp(RunID) lp(OperationID)))
//	attempt_id   = "za-" + hex(SHA-256(lp(RunID) lp(OperationID) lp(Attempt)))
//
// where lp(s) is len(s) in decimal, ":", then s. One host operation is one
// kernel execution, so every physical attempt of it shares one admission
// envelope and a retry can never present a fresh execution_id that escapes
// it; one physical attempt is one attempt_id, so a re-entered attempt is
// refused by admission. The length-prefixed input encoding is injective, so
// it is unambiguous: RunID "a:b" with OperationID "c" never encodes like
// RunID "a" with "b:c". SHA-256 is not injective; it is collision-resistant,
// so two distinct encodings sharing an id would need a SHA-256 collision.
// Every id is a valid kernel identifier of fixed length (67 bytes) whatever
// characters the host's ids contain.
func executionID(ref execution.AttemptRef) string {
	return identity("zx-", ref.RunID, ref.OperationID)
}

func attemptID(ref execution.AttemptRef) string {
	return identity("za-", ref.RunID, ref.OperationID, strconv.Itoa(ref.Attempt))
}

func identity(prefix string, fields ...string) string {
	sum := sha256.New()
	for _, field := range fields {
		fmt.Fprintf(sum, "%d:%s", len(field), field)
	}
	return prefix + hex.EncodeToString(sum.Sum(nil))
}
