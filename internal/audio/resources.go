package audio

import "runtime"

func systemResources() Resources {
	logical := runtime.NumCPU()
	available, total, free := platformResources()
	if available < 1 {
		available = logical
	}
	if available < 1 {
		available = 1
	}
	threads := available - 1
	if threads < 1 {
		threads = 1
	}
	if threads > 32 {
		threads = 32
	}
	return Resources{LogicalCPUs: logical, AvailableCPUs: available, MemoryTotalBytes: total, MemoryAvailableBytes: free, RecommendedThreads: threads, RecommendedModel: "base"}
}
