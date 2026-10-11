//go:build linux

package audio

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func platformResources() (available int, total, free int64) {
	var cpus unix.CPUSet
	if unix.SchedGetaffinity(0, &cpus) == nil {
		available = cpus.Count()
	}
	if quota, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(quota))
		if len(fields) == 2 && fields[0] != "max" {
			limit, e1 := strconv.ParseInt(fields[0], 10, 64)
			period, e2 := strconv.ParseInt(fields[1], 10, 64)
			if e1 == nil && e2 == nil && limit > 0 && period > 0 {
				count := int((limit + period - 1) / period)
				if available == 0 || count < available {
					available = count
				}
			}
		}
	}
	data, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 || value > 1<<50 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value * 1024
		case "MemAvailable:":
			free = value * 1024
		}
	}
	// Containers can expose host RAM while enforcing a smaller memory budget.
	if limitData, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		limit, err := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
		if err == nil && limit > 0 && (total == 0 || limit < total) {
			total = limit
			if currentData, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
				current, err := strconv.ParseInt(strings.TrimSpace(string(currentData)), 10, 64)
				if err == nil && current >= 0 {
					remaining := limit - current
					if remaining < 0 {
						remaining = 0
					}
					if free == 0 || remaining < free {
						free = remaining
					}
				}
			}
		}
	}
	return
}
