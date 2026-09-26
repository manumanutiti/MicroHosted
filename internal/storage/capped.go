package storage

import "os/exec"

// maxToolOutput bounds how much of a host tool's combined output (debugfs,
// e2fsck, resize2fs) the daemon keeps in memory. These tools parse images whose
// contents a guest controls, and a crafted filesystem can make them print
// without end (a directory with millions of entries, an endless stream of fsck
// complaints). Buffering all of it would let a VM grow the daemon's heap until
// the OOM killer steps in — and with the daemon protected by OOMScoreAdjust,
// the kernel kills the other VMs first. The output only ever feeds error
// messages and substring checks near its start, so the head is all we need.
const maxToolOutput = 1 << 20

// cappedBuffer keeps the first max bytes written to it and silently drops the
// rest. Write always reports full success so the child keeps draining its pipe
// instead of blocking (which would turn a flood into a hang until timeout).
type cappedBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			c.truncated = true
		} else {
			c.buf = append(c.buf, p...)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return string(c.buf) + "\n[output truncated]"
	}
	return string(c.buf)
}

// cappedCombinedOutput is exec.Cmd.CombinedOutput with the result bounded to
// maxToolOutput bytes.
func cappedCombinedOutput(cmd *exec.Cmd) (string, error) {
	out := &cappedBuffer{max: maxToolOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.String(), err
}
