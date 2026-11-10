package listen

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testTimeout = 3 * time.Second

func localListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func availableAddress(t *testing.T) string {
	t.Helper()
	ln := localListener(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startRun(t *testing.T, h *HTTP) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("Run did not exit after cancellation")
		}
	})
	return cancel, done
}

func waitForHTTP(t *testing.T, addr, path string, done <-chan error) string {
	t.Helper()
	client := &http.Client{
		Timeout:   200 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	deadline := time.Now().Add(testTimeout)
	for {
		resp, err := client.Get("http://" + addr + path)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s%s: status %d, body %q", addr, path, resp.StatusCode, body)
			}
			return string(body)
		}
		select {
		case runErr := <-done:
			t.Fatalf("Run exited before %s became available: %v", addr, runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s%s never succeeded: %v", addr, path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testTimeout):
		t.Fatal("Run did not exit")
		return nil
	}
}

func TestHTTPMuxConcurrentReuse(t *testing.T) {
	h := NewHTTP()
	const count = 32
	results := make(chan *http.ServeMux, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- h.Mux("127.0.0.1:0")
		}()
	}
	wg.Wait()
	close(results)

	first := <-results
	for mux := range results {
		if mux != first {
			t.Fatal("Mux returned different routers for the same address")
		}
	}
}

func TestHTTPRunNoListenersReturns(t *testing.T) {
	h := NewHTTP()
	_, done := startRun(t, h)
	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "no HTTP listeners") {
		t.Fatalf("Run with no listeners: got %v, want a configuration error", err)
	}
}

func TestHTTPRunCanceledContext(t *testing.T) {
	h := NewHTTP()
	addr := availableAddress(t)
	h.Mux(addr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with a canceled context: got %v, want context.Canceled", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("canceled Run left %s bound: %v", addr, err)
	}
	_ = ln.Close()
}

func TestHTTPRunMultipleListenersCloseAndReuse(t *testing.T) {
	h := NewHTTP()
	a, b := localListener(t), localListener(t)
	addresses := []string{a.Addr().String(), b.Addr().String()}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for i, addr := range addresses {
		message := []string{"first", "second"}[i]
		h.Mux(addr).HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, message)
		})
	}
	if addresses[0] == addresses[1] {
		t.Fatal("expected distinct ephemeral ports")
	}

	cancel, done := startRun(t, h)
	for i, addr := range addresses {
		if got, want := waitForHTTP(t, addr, "/ready", done), []string{"first", "second"}[i]; got != want {
			t.Fatalf("GET %s/ready = %q, want %q", addr, got, want)
		}
	}
	cancel()
	if err := waitForRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation: got %v, want context.Canceled", err)
	}
	for _, addr := range addresses {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("listener %s remains open after Run: %v", addr, err)
		} else {
			_ = ln.Close()
		}
	}

	// Both muxes survive a complete stop/start cycle.
	cancelAgain, doneAgain := startRun(t, h)
	for _, addr := range addresses {
		waitForHTTP(t, addr, "/ready", doneAgain)
	}
	cancelAgain()
	if err := waitForRun(t, doneAgain); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Run after cancellation: got %v, want context.Canceled", err)
	}
}

func TestHTTPMuxWhileRunning(t *testing.T) {
	h := NewHTTP()
	addr := availableAddress(t)
	mux := h.Mux(addr)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	})
	cancel, done := startRun(t, h)
	waitForHTTP(t, addr, "/ready", done)

	otherAddr := availableAddress(t)
	result := make(chan [2]*http.ServeMux, 1)
	go func() {
		result <- [2]*http.ServeMux{h.Mux(addr), h.Mux(otherAddr)}
	}()
	select {
	case got := <-result:
		if got[0] != mux || got[1] == nil || got[1] == mux {
			t.Fatalf("Mux returned unexpected routers: %v", got)
		}
		got[1].HandleFunc("/added", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "added")
		})
	case <-time.After(testTimeout):
		t.Fatal("Mux blocked throughout Run")
	}

	cancel()
	if err := waitForRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation: got %v, want context.Canceled", err)
	}

	cancelAgain, doneAgain := startRun(t, h)
	if got := waitForHTTP(t, otherAddr, "/added", doneAgain); got != "added" {
		t.Fatalf("newly added address returned %q, want added", got)
	}
	cancelAgain()
	if err := waitForRun(t, doneAgain); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Run after cancellation: got %v, want context.Canceled", err)
	}
}

func TestHTTPRunBindFailureReleasesOtherListeners(t *testing.T) {
	a, b := localListener(t), localListener(t)
	// The smaller address binds first; the larger stays occupied.
	if a.Addr().String() > b.Addr().String() {
		a, b = b, a
	}
	available := a.Addr().String()
	occupied := b.Addr().String()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	h := NewHTTP()
	for _, addr := range []string{available, occupied} {
		h.Mux(addr).HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}

	err := h.Run(context.Background())
	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), occupied) {
		t.Fatalf("Run with occupied address: got %v, want bind error for %s", err, occupied)
	}
	ln, err := net.Listen("tcp", available)
	if err != nil {
		t.Fatalf("Run did not release listener %s after bind failure: %v", available, err)
	}
	_ = ln.Close()

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRun(t, h)
	for _, addr := range []string{available, occupied} {
		waitForHTTP(t, addr, "/ready", done)
	}
	cancel()
	if err := waitForRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after bind failure: got %v, want context.Canceled", err)
	}
}

func TestHTTPRunRepeatedBindFailuresDoNotLeakGoroutines(t *testing.T) {
	h := NewHTTP()
	for i := 0; i < 3; i++ {
		h.Mux(localListener(t).Addr().String())
	}
	baseline := runtime.NumGoroutine()
	for i := 0; i < 12; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		err := h.Run(ctx)
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run %d: got %v, want bind error", i, err)
		}
	}
	deadline := time.Now().Add(testTimeout)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+4 {
		t.Fatalf("repeated bind failures leaked goroutines: before %d, after %d", baseline, got)
	}
}
