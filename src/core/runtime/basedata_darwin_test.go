//go:build darwin && (amd64 || arm64) && !tinygo

package runtime

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestAcquireJobMemoryGrowableZeroesAfterChildProcess(t *testing.T) {
	const size = 1 << 20 // Exercise reclamation above the in-place clear threshold.
	jm, err := AcquireJobMemoryGrowable(size, size)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if jm != nil {
			if err := jm.Close(); err != nil {
				t.Errorf("close job memory: %v", err)
			}
		}
	}()
	lin := jm.CurrentBytes()
	for i := range lin {
		lin[i] = 0xa5
	}

	// Darwin's fork/exec path can leave the parent's mapping copy-on-write,
	// which MADV_ZERO does not support, even after the child exits.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "/usr/bin/true").CombinedOutput(); err != nil {
		t.Fatalf("run child: %v\n%s", err, output)
	}

	previous := jm
	jm = nil
	if err := ReleaseJobMemory(previous); err != nil {
		t.Fatalf("release job memory: %v", err)
	}
	jm, err = AcquireJobMemoryGrowable(size, size)
	if err != nil {
		t.Fatalf("reacquire job memory: %v", err)
	}
	for i, b := range jm.CurrentBytes() {
		if b != 0 {
			t.Fatalf("reused linear memory byte %d = %#x, want 0", i, b)
		}
	}
}
