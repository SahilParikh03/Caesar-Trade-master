package signer

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	signerv1 "github.com/caesar-terminal/caesar/internal/gen/signer/v1"
	"google.golang.org/grpc"
)

// Server wraps the gRPC server and its Unix Domain Socket listener.
type Server struct {
	grpcServer *grpc.Server
	listener   net.Listener
	socketPath string
}

// New creates a new Signer gRPC server bound to the given UDS path.
// It registers the SignerService handler and prepares the listener.
func New(socketPath string, session *SessionManager) (*Server, error) {
	// Ensure the socket directory exists.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	// Remove any stale socket file from a previous run.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket %s: %w", socketPath, err)
	}

	// Restrict socket permissions to owner only.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		lis.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	gs := grpc.NewServer()
	handler := NewHandler(session)
	signerv1.RegisterSignerServiceServer(gs, handler)

	return &Server{
		grpcServer: gs,
		listener:   lis,
		socketPath: socketPath,
	}, nil
}

// Serve starts accepting gRPC connections. It blocks until the server
// is stopped or an error occurs.
func (s *Server) Serve() error {
	return s.grpcServer.Serve(s.listener)
}

// GracefulStop gracefully drains in-flight RPCs and cleans up the socket file.
func (s *Server) GracefulStop() {
	s.grpcServer.GracefulStop()
	os.Remove(s.socketPath)
}
