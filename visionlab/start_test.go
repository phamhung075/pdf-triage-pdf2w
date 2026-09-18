package visionlab

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeListener blocks in Accept until Close, so http.Server.Serve exits on shutdown.
type fakeListener struct {
	closed chan struct{}
	once   sync.Once
}

func newFakeListener() *fakeListener { return &fakeListener{closed: make(chan struct{})} }

func (l *fakeListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

func noopLogf(string, ...any) {}

func TestStartServesAndShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listener := newFakeListener()
	started := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)

	go func() {
		done <- Start(ctx, "127.0.0.1:0", Deps{
			Stepper: newFakeStepper(),
			Listen:  func(string, string) (net.Listener, error) { return listener, nil },
			Exit:    func(int) { t.Error("Exit must not run on a clean shutdown") },
			Logf: func(format string, _ ...any) {
				if format == "Vision Lab server running at http://%s" {
					once.Do(func() { close(started) })
				}
			},
		})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("server never reported that it started")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestStartTakesOverPortOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := newFakeListener()
	var attempts atomic.Int64
	var killedPort atomic.Int64
	started := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)

	go func() {
		done <- Start(ctx, "127.0.0.1:3979", Deps{
			Stepper: newFakeStepper(),
			Listen: func(string, string) (net.Listener, error) {
				if attempts.Add(1) == 1 {
					return nil, syscall.EADDRINUSE
				}
				return listener, nil
			},
			KillProcessOnPort: func(port int) bool { killedPort.Store(int64(port)); return true },
			Sleep:             func(time.Duration) {},
			Exit:              func(int) { t.Error("Exit must not run after a successful takeover") },
			Logf: func(format string, _ ...any) {
				if format == "Vision Lab server running at http://%s" {
					once.Do(func() { close(started) })
				}
			},
		})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("takeover retry never started the server")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	if attempts.Load() != 2 {
		t.Fatalf("listen attempts = %d, want 2", attempts.Load())
	}
	if killedPort.Load() != 3979 {
		t.Fatalf("killed port = %d, want 3979", killedPort.Load())
	}
}

func TestStartFailsFastAfterTakeoverRetry(t *testing.T) {
	var attempts atomic.Int64
	var exits []int
	err := Start(context.Background(), "127.0.0.1:3979", Deps{
		Stepper:           newFakeStepper(),
		Listen:            func(string, string) (net.Listener, error) { attempts.Add(1); return nil, syscall.EADDRINUSE },
		KillProcessOnPort: func(int) bool { return true },
		Sleep:             func(time.Duration) {},
		Exit:              func(code int) { exits = append(exits, code) },
		Logf:              noopLogf,
	})
	if err == nil {
		t.Fatal("Start returned nil, want an error")
	}
	if attempts.Load() != 2 {
		t.Fatalf("listen attempts = %d, want 2", attempts.Load())
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}

func TestStartFailsWhenNoProcessCouldBeFound(t *testing.T) {
	var attempts atomic.Int64
	var exits []int
	killed := false
	err := Start(context.Background(), "127.0.0.1:3979", Deps{
		Stepper: newFakeStepper(),
		Listen: func(string, string) (net.Listener, error) {
			attempts.Add(1)
			return nil, syscall.EADDRINUSE
		},
		KillProcessOnPort: func(int) bool { killed = true; return false },
		Sleep:             func(time.Duration) {},
		Exit:              func(code int) { exits = append(exits, code) },
		Logf:              noopLogf,
	})
	if err == nil {
		t.Fatal("Start returned nil, want an error")
	}
	if !killed {
		t.Fatal("KillProcessOnPort was not called")
	}
	if attempts.Load() != 1 {
		t.Fatalf("listen attempts = %d, want 1 (no retry when nothing was killed)", attempts.Load())
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}

func TestStartFailsOnOtherListenError(t *testing.T) {
	var exits []int
	boom := errors.New("boom")
	err := Start(context.Background(), "127.0.0.1:3979", Deps{
		Stepper:           newFakeStepper(),
		Listen:            func(string, string) (net.Listener, error) { return nil, boom },
		KillProcessOnPort: func(int) bool { return true },
		Exit:              func(code int) { exits = append(exits, code) },
		Logf:              noopLogf,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}
