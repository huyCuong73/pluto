package pqcproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Server quản lý vòng đời companion proxy và chỉ cho phép listen loopback để
// private signing capability không vô tình bị mở cho máy khác trong mạng.
type Server struct {
	proxy      *Proxy
	httpServer *http.Server
	listener   net.Listener
	done       chan error
}

func NewServer(config Config) (*Server, error) {
	proxy, err := New(config)
	if err != nil {
		return nil, err
	}
	return &Server{proxy: proxy, done: make(chan error, 1)}, nil
}

func (server *Server) Start(address string) error {
	if !isLoopbackAddress(address) {
		return fmt.Errorf("PQC proxy chỉ được listen trên loopback, got %q", address)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server.listener = listener
	server.httpServer = &http.Server{
		Handler:           server.proxy,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		err := server.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.done <- err
	}()
	return nil
}

func (server *Server) Addr() net.Addr {
	if server == nil || server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

func (server *Server) Done() <-chan error { return server.done }

func (server *Server) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	defer server.proxy.Close()
	if server.httpServer == nil {
		return nil
	}
	return server.httpServer.Shutdown(ctx)
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
