package httpguard

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientIPTrustBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, remote string
		real         []string
		want         string
	}{
		{"proxy-v4", "127.0.0.1:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"proxy-v6", "[::1]:1234", []string{"2001:db8::1"}, "2001:db8::1"},
		{"canonical", "127.0.0.1:1234", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"remote-spoof", "198.51.100.1:1234", []string{"203.0.113.1"}, "198.51.100.1"},
		{"missing-header", "127.0.0.1:1234", nil, "127.0.0.1"},
		{"multiple-values", "127.0.0.1:1234", []string{"198.51.100.1", "203.0.113.1"}, "127.0.0.1"},
		{"comma-chain", "127.0.0.1:1234", []string{"198.51.100.1,203.0.113.1"}, "127.0.0.1"},
		{"port", "127.0.0.1:1234", []string{"198.51.100.1:5678"}, "127.0.0.1"},
		{"zone", "127.0.0.1:1234", []string{"fe80::1%eth0"}, "127.0.0.1"},
		{"invalid-peer", "malformed", []string{"198.51.100.1"}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.RemoteAddr = tc.remote
			r.Header["X-Real-Ip"] = tc.real
			r.Header.Set("X-Forwarded-For", "192.0.2.100")
			r.Header.Set("Forwarded", "for=192.0.2.100")
			if got := ClientIP(r); got != tc.want {
				t.Fatalf("client = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLimiterBudgetsAreAtomicBoundedAndRecover(t *testing.T) {
	l := NewLimiter(10, 25, time.Minute)
	now := time.Now()
	var accepted atomic.Int32
	var requests sync.WaitGroup
	for range 100 {
		requests.Go(func() {
			if l.Allow("same", now) {
				accepted.Add(1)
			}
		})
	}
	requests.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("per-client budget admitted %d", accepted.Load())
	}
	for i := range 1000 {
		if l.Allow(fmt.Sprint(i), now) {
			accepted.Add(1)
		}
	}
	if accepted.Load() != 25 || len(l.clients) != 16 {
		t.Fatalf("global budget or state bound: admitted=%d clients=%d", accepted.Load(), len(l.clients))
	}
	if !l.Allow("same", now.Add(time.Minute)) || len(l.clients) != 1 {
		t.Fatal("limiter did not expire its window and bounded state")
	}
}
