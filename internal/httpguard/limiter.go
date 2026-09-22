package httpguard

import (
	"sync"
	"time"
)

// Limiter admits a fixed-window per-client and global request budget. Rejected
// requests neither allocate state nor consume another client's global budget;
// the map is bounded by the global budget and discarded once per window.
type Limiter struct {
	mu                       sync.Mutex
	window                   time.Duration
	clientLimit, globalLimit int
	since                    time.Time
	count                    int
	clients                  map[string]int
}

func NewLimiter(clientLimit, globalLimit int, window time.Duration) *Limiter {
	return &Limiter{clientLimit: clientLimit, globalLimit: globalLimit, window: window}
}

func (l *Limiter) Allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clients == nil || now.Sub(l.since) >= l.window {
		l.since, l.count = now, 0
		l.clients = make(map[string]int)
	}
	if l.count >= l.globalLimit || l.clients[client] >= l.clientLimit {
		return false
	}
	l.clients[client]++
	l.count++
	return true
}
