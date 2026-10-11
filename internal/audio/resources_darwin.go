//go:build darwin

package audio

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func platformResources() (available int, total, free int64) {
	if count, err := unix.SysctlUint32("hw.logicalcpu"); err == nil {
		available = int(count)
	}
	if memory, err := unix.SysctlUint64("hw.memsize"); err == nil && memory < 1<<63 {
		total = int64(memory)
	}
	// vm_stat reports reclaimable pages without requiring elevated privileges.
	// Invoke one fixed OS utility directly with no shell and a bounded deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/vm_stat")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return
	}
	free = availableMemoryFromVMStat(string(output))
	return
}

func availableMemoryFromVMStat(output string) int64 {
	pageSize := int64(4096)
	var pages int64
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "page size of ") {
			_, suffix, _ := strings.Cut(line, "page size of ")
			fields := strings.Fields(suffix)
			if len(fields) > 0 {
				if size, err := strconv.ParseInt(fields[0], 10, 64); err == nil && size > 0 && size < 1<<20 {
					pageSize = size
				}
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || (name != "Pages free" && name != "Pages inactive" && name != "Pages speculative") {
			continue
		}
		count, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err == nil && count >= 0 && count < 1<<40 {
			pages += count
		}
	}
	return pages * pageSize
}
