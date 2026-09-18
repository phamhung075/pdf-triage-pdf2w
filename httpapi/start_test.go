package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
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

func TestStartRefusesWhenLockHeld(t *testing.T) {
	dataDir := t.TempDir()
	var lockPath string
	var exits []int
	acquired := false

	err := Start(context.Background(), "127.0.0.1:0", StartOptions{
		Handler: http.NotFoundHandler(),
		DataDir: dataDir,
		ReadActiveLockHolder: func(path string) (int, bool) {
			lockPath = path
			return 123, true
		},
		AcquireProcessLock: func(string) (func(), error) {
			acquired = true
			return func() {}, nil
		},
		Exit:  func(code int) { exits = append(exits, code) },
		Logf:  noopLogf,
		Sleep: func(time.Duration) {},
		Listen: func(string, string) (net.Listener, error) {
			t.Fatal("must not listen when the lock is held")
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("Start returned nil, want an error")
	}
	if acquired {
		t.Fatal("AcquireProcessLock ran despite a live holder")
	}
	if want := filepath.Join(dataDir, ".server.lock"); lockPath != want {
		t.Fatalf("lock path = %q, want %q", lockPath, want)
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}

func TestStartServesAndReleasesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listener := newFakeListener()
	releaseCalled := false
	done := make(chan error, 1)

	go func() {
		done <- Start(ctx, "127.0.0.1:0", StartOptions{
			Handler:              http.NotFoundHandler(),
			DataDir:              t.TempDir(),
			ReadActiveLockHolder: func(string) (int, bool) { return 0, false },
			AcquireProcessLock: func(string) (func(), error) {
				return func() { releaseCalled = true }, nil
			},
			Listen: func(string, string) (net.Listener, error) { return listener, nil },
			Exit:   func(int) { t.Error("Exit must not run on a clean shutdown") },
			Logf:   noopLogf,
		})
	}()

	waitFor(t, "server to start", func() bool { return listenerClosed(listener) == false })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	if !releaseCalled {
		t.Fatal("lock release was not called")
	}
}

func listenerClosed(l *fakeListener) bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

func TestStartTakesOverPortOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := newFakeListener()
	var attempts atomic.Int64
	var killedPort atomic.Int64
	done := make(chan error, 1)

	go func() {
		done <- Start(ctx, "127.0.0.1:3971", StartOptions{
			Handler:              http.NotFoundHandler(),
			DataDir:              t.TempDir(),
			ReadActiveLockHolder: func(string) (int, bool) { return 0, false },
			AcquireProcessLock:   func(string) (func(), error) { return func() {}, nil },
			Listen: func(string, string) (net.Listener, error) {
				if attempts.Add(1) == 1 {
					return nil, syscall.EADDRINUSE
				}
				return listener, nil
			},
			KillProcessOnPort: func(port int) bool { killedPort.Store(int64(port)); return true },
			Sleep:             func(time.Duration) {},
			Exit:              func(int) { t.Error("Exit must not run after a successful takeover") },
			Logf:              noopLogf,
		})
	}()

	waitFor(t, "takeover retry", func() bool { return attempts.Load() >= 2 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	if killedPort.Load() != 3971 {
		t.Fatalf("killed port = %d, want 3971", killedPort.Load())
	}
}

func TestStartFailsWhenPortStillInUse(t *testing.T) {
	var exits []int
	err := Start(context.Background(), "127.0.0.1:3971", StartOptions{
		Handler:              http.NotFoundHandler(),
		DataDir:              t.TempDir(),
		ReadActiveLockHolder: func(string) (int, bool) { return 0, false },
		AcquireProcessLock:   func(string) (func(), error) { return func() {}, nil },
		Listen:               func(string, string) (net.Listener, error) { return nil, syscall.EADDRINUSE },
		KillProcessOnPort:    func(int) bool { return false },
		Sleep:                func(time.Duration) {},
		Exit:                 func(code int) { exits = append(exits, code) },
		Logf:                 noopLogf,
	})
	if err == nil {
		t.Fatal("Start returned nil, want an error")
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}

func TestStartFailsOnOtherListenError(t *testing.T) {
	var exits []int
	boom := errors.New("boom")
	err := Start(context.Background(), "127.0.0.1:3971", StartOptions{
		Handler:              http.NotFoundHandler(),
		DataDir:              t.TempDir(),
		ReadActiveLockHolder: func(string) (int, bool) { return 0, false },
		AcquireProcessLock:   func(string) (func(), error) { return func() {}, nil },
		Listen:               func(string, string) (net.Listener, error) { return nil, boom },
		Exit:                 func(code int) { exits = append(exits, code) },
		Logf:                 noopLogf,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatalf("exits = %v, want [1]", exits)
	}
}

func TestStartReturnsLockAcquireError(t *testing.T) {
	boom := errors.New("lock boom")
	err := Start(context.Background(), "127.0.0.1:0", StartOptions{
		Handler:              http.NotFoundHandler(),
		DataDir:              t.TempDir(),
		ReadActiveLockHolder: func(string) (int, bool) { return 0, false },
		AcquireProcessLock:   func(string) (func(), error) { return nil, boom },
		Exit:                 func(int) {},
		Logf:                 noopLogf,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want lock boom", err)
	}
}
