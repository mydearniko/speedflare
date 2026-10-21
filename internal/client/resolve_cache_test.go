package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveCacheSharesConcurrentLookups(t *testing.T) {
	var cache resolveCache
	var calls atomic.Int32
	release := make(chan struct{})
	resolve := func(context.Context) ([]net.IP, error) {
		calls.Add(1)
		<-release
		return []net.IP{net.ParseIP("192.0.2.1")}, nil
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ips, err := cache.lookup(context.Background(), "host", resolve)
			if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("192.0.2.1")) {
				t.Errorf("lookup = %v, %v", ips, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if _, err := cache.lookup(context.Background(), "host", resolve); err != nil {
		t.Fatalf("cached lookup failed: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolve calls = %d, want 1", got)
	}
}

func TestResolveCacheRetriesAfterFailureAndInvalidate(t *testing.T) {
	var cache resolveCache
	var calls atomic.Int32
	resolve := func(context.Context) ([]net.IP, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("temporary")
		}
		return []net.IP{net.ParseIP("192.0.2.2")}, nil
	}

	if _, err := cache.lookup(context.Background(), "host", resolve); err == nil {
		t.Fatal("expected first lookup to fail")
	}
	if _, err := cache.lookup(context.Background(), "host", resolve); err != nil {
		t.Fatalf("failure was cached: %v", err)
	}
	cache.invalidate("host")
	if _, err := cache.lookup(context.Background(), "host", resolve); err != nil {
		t.Fatalf("lookup after invalidate failed: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("resolve calls = %d, want 3", got)
	}
}

func TestResolveCacheCallerCancelDoesNotAbortSharedLookup(t *testing.T) {
	var cache resolveCache
	release := make(chan struct{})
	resolveErr := make(chan error, 1)
	resolve := func(ctx context.Context) ([]net.IP, error) {
		<-release
		resolveErr <- ctx.Err()
		return []net.IP{net.ParseIP("192.0.2.3")}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.lookup(ctx, "host", resolve); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller err = %v, want context.Canceled", err)
	}

	waiter := make(chan error, 1)
	go func() {
		_, err := cache.lookup(context.Background(), "host", resolve)
		waiter <- err
	}()
	close(release)
	if err := <-resolveErr; err != nil {
		t.Fatalf("shared resolve saw canceled context: %v", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("second caller failed: %v", err)
	}
}
