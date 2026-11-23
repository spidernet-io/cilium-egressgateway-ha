package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx := context.Background()
	if err := Once(ctx, ln.Addr().String()); err != nil {
		t.Errorf("listening port: want alive, got %v", err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()
	if err := Once(ctx, closedAddr); err != nil {
		t.Errorf("refused port: want alive, got %v", err)
	}

	// An unroutable address is not used here: hosts with a transparent proxy
	// accept every TCP connection. An expired context simulates the timeout.
	tctx, cancel := context.WithTimeout(ctx, -time.Second)
	defer cancel()
	if err := Once(tctx, ln.Addr().String()); err == nil {
		t.Error("timeout: want failure, got alive")
	}
}

func TestClassify(t *testing.T) {
	opErr := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	for name, tc := range map[string]struct {
		err   error
		alive bool
	}{
		"connected":    {nil, true},
		"refused":      {opErr(syscall.ECONNREFUSED), true},
		"host unreach": {opErr(syscall.EHOSTUNREACH), false},
		"net unreach":  {opErr(syscall.ENETUNREACH), false},
		"timeout":      {context.DeadlineExceeded, false},
	} {
		if got := classify(tc.err) == nil; got != tc.alive {
			t.Errorf("%s: alive = %v, want %v", name, got, tc.alive)
		}
	}
}

type fakeDialer struct {
	mu      sync.Mutex
	errs    map[string][]error // consumed in order; the last one repeats
	calls   map[string]int
	active  atomic.Int32
	maxSeen atomic.Int32
	block   time.Duration
}

func (f *fakeDialer) dial(ctx context.Context, addr string) error {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		m := f.maxSeen.Load()
		if n <= m || f.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	if f.block > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(f.block):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[addr]++
	seq := f.errs[addr]
	if len(seq) == 0 {
		return nil
	}
	err := seq[0]
	if len(seq) > 1 {
		f.errs[addr] = seq[1:]
	}
	return err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var errTimeout = errors.New("i/o timeout")

func TestProberThresholdAndRecovery(t *testing.T) {
	cfg := Config{Port: 10250, Timeout: 50 * time.Millisecond, Interval: 5 * time.Millisecond, FailureThreshold: 3}
	f := &fakeDialer{calls: map[string]int{}, errs: map[string][]error{
		// 2 failures (below threshold), success, then failing forever.
		"10.0.0.1:10250": {errTimeout, errTimeout, nil, errTimeout},
	}}
	var changes atomic.Int32
	p := New(cfg, f.dial, func(string) { changes.Add(1) })
	defer p.Stop()

	if _, ok := p.Get("n1"); ok {
		t.Fatal("unknown target should not have a result")
	}
	p.SetTargets(map[string]netip.Addr{"n1": netip.MustParseAddr("10.0.0.1")})
	r, ok := p.Get("n1")
	if !ok || r.Probed || r.Reachable() || r.Failed() {
		t.Fatalf("initial result = %+v, want unprobed and not failed", r)
	}

	var history []int
	waitFor(t, "threshold reached", func() bool {
		r, _ := p.Get("n1")
		if n := len(history); r.Probed && (n == 0 || history[n-1] != r.ConsecutiveFailures) {
			history = append(history, r.ConsecutiveFailures)
		}
		return r.Failed()
	})
	r, _ = p.Get("n1")
	if r.Reachable() || r.LastError != errTimeout.Error() {
		t.Errorf("failed result = %+v", r)
	}
	if want := "3/3 probes to 10.0.0.1:10250 failed: i/o timeout"; r.Message() != want {
		t.Errorf("Message() = %q, want %q", r.Message(), want)
	}
	// The success must have reset the counter before the final run of failures.
	sawReset := false
	for i := 1; i < len(history); i++ {
		if history[i] < history[i-1] {
			sawReset = true
		}
	}
	if !sawReset {
		t.Errorf("failure history %v never reset after a success", history)
	}
	if changes.Load() < 4 {
		t.Errorf("onChange called %d times, want at least 4", changes.Load())
	}
}

func TestProberSerialWhenTimeoutExceedsInterval(t *testing.T) {
	cfg := Config{Port: 1, Timeout: 30 * time.Millisecond, Interval: time.Millisecond, FailureThreshold: 1}
	f := &fakeDialer{calls: map[string]int{}, errs: map[string][]error{}, block: time.Hour}
	p := New(cfg, f.dial, nil)
	p.SetTargets(map[string]netip.Addr{"n1": netip.MustParseAddr("10.0.0.1")})
	time.Sleep(150 * time.Millisecond)
	p.Stop()
	if got := f.maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent probes = %d, want 1", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["10.0.0.1:1"] < 2 {
		t.Errorf("probes = %d, want at least 2", f.calls["10.0.0.1:1"])
	}
}

func TestProberSetTargets(t *testing.T) {
	cfg := Config{Port: 10250, Timeout: 50 * time.Millisecond, Interval: 5 * time.Millisecond, FailureThreshold: 1}
	f := &fakeDialer{calls: map[string]int{}, errs: map[string][]error{
		"10.0.0.1:10250": {errTimeout},
	}}
	p := New(cfg, f.dial, nil)
	defer p.Stop()

	p.SetTargets(map[string]netip.Addr{
		"n1": netip.MustParseAddr("10.0.0.1"),
		"n2": netip.MustParseAddr("10.0.0.2"),
	})
	waitFor(t, "n1 failed and n2 reachable", func() bool {
		r1, _ := p.Get("n1")
		r2, _ := p.Get("n2")
		return r1.Failed() && r2.Reachable()
	})

	// Changing the IP of n1 restarts it with a fresh result; removing n2 stops it.
	p.SetTargets(map[string]netip.Addr{"n1": netip.MustParseAddr("10.0.0.3")})
	if _, ok := p.Get("n2"); ok {
		t.Error("n2 should be removed")
	}
	waitFor(t, "n1 reachable on new IP", func() bool {
		r, _ := p.Get("n1")
		return r.Reachable() && r.Addr == "10.0.0.3:10250"
	})
	f.mu.Lock()
	n2Calls := f.calls["10.0.0.2:10250"]
	f.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["10.0.0.2:10250"] != n2Calls {
		t.Error("removed target is still being probed")
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
	for _, c := range []Config{
		{Port: 0, Timeout: time.Second, Interval: time.Second, FailureThreshold: 1},
		{Port: 1, Timeout: 0, Interval: time.Second, FailureThreshold: 1},
		{Port: 1, Timeout: time.Second, Interval: time.Second, FailureThreshold: 0},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", c)
		}
	}
}
