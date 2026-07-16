// Process-ancestry primitives shared by the registry poller and the bridge
// pointer watcher. Both joins rest on the same verified fact: claude sessions
// run as descendant processes of the pid GroundControl spawned, so ownership
// is a bounded walk up one ps snapshot. Lives here rather than claudex because
// ps is a general process query, not a claude state query.
package sessions

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxAncestryDepth bounds the ancestor walk; the bound doubles as cycle
// safety, since pid reuse in a torn snapshot can point a ppid chain at itself.
const maxAncestryDepth = 10

// execPSParents builds a pid→ppid map with a single ps exec.
func execPSParents(timeout time.Duration) (map[int]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	// a killed ps must not wedge Wait on an inherited pipe (gitx/claudex rule)
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return parsePSOutput(out.String()), nil
}

// parsePSOutput turns `ps -axo pid=,ppid=` output into a pid→ppid map. Torn
// lines (wrong field count, non-integer fields) are skipped, never an error —
// a degraded snapshot is better than a failed join. Split out from the exec so
// the parse is testable without a real ps.
func parsePSOutput(out string) map[int]int {
	ps := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, errPid := strconv.Atoi(fields[0])
		ppid, errPpid := strconv.Atoi(fields[1])
		if errPid != nil || errPpid != nil {
			continue // torn line — skip, never error
		}
		ps[pid] = ppid
	}
	return ps
}

// reachesAncestor reports whether walking pid's ppid chain hits ancestor.
func reachesAncestor(ps map[int]int, pid, ancestor int) bool {
	if pid <= 0 || ancestor <= 0 {
		return false
	}
	for i := 0; i <= maxAncestryDepth; i++ {
		if pid == ancestor {
			return true
		}
		next, ok := ps[pid]
		if !ok || next == pid || next <= 0 {
			return false
		}
		pid = next
	}
	return false
}
