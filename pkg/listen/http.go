package listen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

type HTTP struct {
	sync.Mutex
	mp map[string]*http.ServeMux
}

func NewHTTP() *HTTP {
	return &HTTP{
		mp: make(map[string]*http.ServeMux),
	}
}

func (h *HTTP) Mux(addr string) *http.ServeMux {
	h.Lock()
	defer h.Unlock()

	mux, exists := h.mp[addr]
	if exists {
		return mux
	}

	mux = http.NewServeMux()
	h.mp[addr] = mux
	return mux
}

func (h *HTTP) Run(ctx context.Context) error {
	// A run serves a snapshot of the configured addresses. Mux can be used
	// concurrently; newly added addresses take effect on the next run.
	h.Lock()
	muxes := make(map[string]*http.ServeMux, len(h.mp))
	addresses := make([]string, 0, len(h.mp))
	for addr, mux := range h.mp {
		muxes[addr] = mux
		addresses = append(addresses, addr)
	}
	h.Unlock()

	if len(addresses) == 0 {
		return errors.New("no HTTP listeners configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	sort.Strings(addresses)
	servers := make([]*http.Server, 0, len(addresses))
	listeners := make([]net.Listener, 0, len(addresses))
	for _, addr := range addresses {
		if err := ctx.Err(); err != nil {
			for _, ln := range listeners {
				_ = ln.Close()
			}
			return err
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err != nil {
			for _, ln := range listeners {
				_ = ln.Close()
			}
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
		servers = append(servers, &http.Server{
			Addr:        addr,
			Handler:     muxes[addr],
			ReadTimeout: 10 * time.Second,
		})
	}

	errChan := make(chan error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server, ln net.Listener) {
			defer wg.Done()
			errChan <- srv.Serve(ln)
		}(srv, listeners[i])
	}

	var result error
	select {
	case result = <-errChan:
	case <-ctx.Done():
		result = ctx.Err()
	}
	for _, srv := range servers {
		_ = srv.Close()
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	wg.Wait()
	return result
}
