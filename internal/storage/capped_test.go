package storage

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCappedCombinedOutputBoundsFlood: a tool that prints without end (as a
// crafted image can make debugfs do) must cost at most maxToolOutput bytes and
// still run to completion rather than block on a full pipe.
func TestCappedCombinedOutputBoundsFlood(t *testing.T) {
	cmd := exec.Command("sh", "-c", "head -c 8388608 /dev/zero | tr '\\0' A")
	out, err := cappedCombinedOutput(cmd)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	if len(out) > maxToolOutput+64 {
		t.Fatalf("kept %d bytes, cap is %d", len(out), maxToolOutput)
	}
	if !strings.HasSuffix(out, "[output truncated]") {
		t.Fatal("truncation not reported")
	}
}

func TestCappedCombinedOutputSmall(t *testing.T) {
	out, err := cappedCombinedOutput(exec.Command("sh", "-c", "echo hi; echo err >&2"))
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi\nerr\n" {
		t.Fatalf("got %q", out)
	}
}
