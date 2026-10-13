package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"
)

const (
	Addr      = ":8080"
	DebugAddr = ":9464"
)

func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// Debug serves the metrics and pprof on a port that compose does not publish
func Debug(addr string, metrics http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return NewServer(addr, mux)
}

func Run(ctx context.Context, servers ...*http.Server) error {
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errs <- s.ListenAndServe() }()
	}
	var first error
	record := func(err error) {
		if first == nil && !errors.Is(err, http.ErrServerClosed) {
			first = err
		}
	}
	pending := len(servers)
	select {
	case <-ctx.Done():
	case err := <-errs:
		record(err)
		pending--
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() { _ = s.Shutdown(stop) })
	}
	wg.Wait()
	for ; pending > 0; pending-- {
		record(<-errs)
	}
	return first
}
