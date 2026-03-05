package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
)

// BackendPool manages HTTP health-check backends on specific ports.
type BackendPool struct {
	mu        sync.Mutex
	listeners map[int]net.Listener // port -> listener
	servers   map[int]*http.Server
}

func NewBackendPool() *BackendPool {
	return &BackendPool{
		listeners: make(map[int]net.Listener),
		servers:   make(map[int]*http.Server),
	}
}

// Start launches an HTTP backend on the given port.
// It responds 200 to /healthz and 404 to everything else.
func (bp *BackendPool) Start(port int) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if _, ok := bp.listeners[port]; ok {
		return fmt.Errorf("backend already running on port %d", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	lis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on %d: %w", port, err)
	}

	srv := &http.Server{Handler: mux}
	bp.listeners[port] = lis
	bp.servers[port] = srv

	go func() {
		if err := srv.Serve(lis); err != http.ErrServerClosed {
			log.Printf("backend %d: %v", port, err)
		}
	}()
	return nil
}

// Stop shuts down the backend on the given port.
func (bp *BackendPool) Stop(port int) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	srv, ok := bp.servers[port]
	if !ok {
		return fmt.Errorf("no backend on port %d", port)
	}
	err := srv.Shutdown(context.Background())
	delete(bp.listeners, port)
	delete(bp.servers, port)
	return err
}

// StopAll shuts down all backends.
func (bp *BackendPool) StopAll() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for port, srv := range bp.servers {
		srv.Shutdown(context.Background())
		delete(bp.listeners, port)
		delete(bp.servers, port)
	}
}

// IsRunning returns whether a backend is active on the given port.
func (bp *BackendPool) IsRunning(port int) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	_, ok := bp.listeners[port]
	return ok
}
