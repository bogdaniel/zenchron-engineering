package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A noisy child process cannot make the controller hold its whole output.
//
// The capture was a strings.Builder: whatever a provider printed, this process
// held, then wrote to a transcript, then read back whole on the next attempt.
// Nothing in the runtime bounds how much a provider - or a test loop it starts
// - decides to print.
func TestAChildProcessCannotFillMemoryThroughItsOutput(t *testing.T) {
	original := maxCapturedProcessBytes
	maxCapturedProcessBytes = 1 << 10
	defer func() { maxCapturedProcessBytes = original }()

	// 64 KiB of stdout against a 1 KiB bound, from a real process through the
	// real executor.
	out, err := OSCommandExecutor{}.Run(context.Background(), "sh",
		[]string{"-c", "yes abcdefghijklmnopqrstuvwxyz | head -c 65536"},
		t.TempDir(), nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	captured := string(out.Stdout)
	if len(captured) > maxCapturedProcessBytes+256 {
		t.Fatalf("captured %d bytes against a %d-byte bound", len(captured), maxCapturedProcessBytes)
	}
	// The transcript must not read as a complete record of what was said.
	if !strings.Contains(captured, "truncated by Zenchron") {
		t.Fatalf("truncation was silent: %q", captured[max(0, len(captured)-120):])
	}
	if !strings.Contains(captured, "further bytes were produced") {
		t.Fatalf("the notice does not say what was lost: %q", captured[max(0, len(captured)-120):])
	}
}

// The bound cuts on a rune boundary, like every other bound in this package.
func TestABoundedCaptureNeverSplitsARune(t *testing.T) {
	buffer := &boundedBuffer{limit: 9}
	if _, err := buffer.Write([]byte("abcdefgh" + strings.Repeat("ă", 4))); err != nil {
		t.Fatal(err)
	}
	captured, _, _ := strings.Cut(string(buffer.Bytes()), "\n[truncated")
	if captured != "abcdefgh" {
		t.Fatalf("the capture cut through a character: %q", captured)
	}
	// Reading twice returns the same bytes. Appending the notice onto the
	// buffer's own array would write it over what was trimmed.
	if second, _, _ := strings.Cut(string(buffer.Bytes()), "\n[truncated"); second != captured {
		t.Fatalf("a second read returned %q, want %q", second, captured)
	}
}

// A complete capture is returned byte for byte.
//
// The rune trim ran unconditionally, so output that reached no bound at all -
// a transcript legitimately ending in U+FFFD, or binary output ending on a
// lead byte - lost one to three real bytes, silently, while the code claimed
// such output was left exactly as captured. And where the trim IS right, the
// bytes it removes are bytes the caller does not get: counting only the
// refused writes made the notice's exact number short by up to three.
func TestACompleteCaptureIsNotTrimmed(t *testing.T) {
	for name, body := range map[string]string{
		"a legitimate replacement character": "abc�",
		"binary ending on a lead byte":       string([]byte{0x61, 0x62, 0xC4}),
	} {
		buffer := &boundedBuffer{limit: 64}
		if _, err := buffer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if got := string(buffer.Bytes()); got != body {
			t.Fatalf("%s: capture returned %q, want %q", name, got, body)
		}
	}
}

func TestTheTruncationNoticeCountsTheBytesTheTrimRemoved(t *testing.T) {
	// Eight ASCII bytes then four two-byte characters, bounded to nine: the
	// ninth byte is a lead byte the trim removes, and the remaining seven were
	// refused outright.
	buffer := &boundedBuffer{limit: 9}
	if _, err := buffer.Write([]byte("abcdefgh" + strings.Repeat("ă", 4))); err != nil {
		t.Fatal(err)
	}
	out := string(buffer.Bytes())
	kept, notice, found := strings.Cut(out, "\n[truncated by Zenchron: ")
	if !found {
		t.Fatalf("no truncation notice: %q", out)
	}
	if kept != "abcdefgh" {
		t.Fatalf("kept %q", kept)
	}
	if !strings.HasPrefix(notice, "8 further bytes") {
		t.Fatalf("the notice does not account for the trimmed byte: %q", notice)
	}
}
