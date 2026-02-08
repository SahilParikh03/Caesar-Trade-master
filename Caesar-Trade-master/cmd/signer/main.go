package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/awnumar/memguard"
	"github.com/caesar-terminal/caesar/internal/config"
	"github.com/caesar-terminal/caesar/internal/signer"
)

func main() {
	defer memguard.Purge()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Caesar Signer starting (env=%s, socket=%s)\n", cfg.Env, cfg.Signer.SocketPath)

	ttl := time.Duration(cfg.Signer.SessionTTLSec) * time.Second
	session := signer.NewSessionManager(ttl)

	srv, err := signer.New(cfg.Signer.SocketPath, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create signer server: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run gRPC server in a goroutine so we can wait for shutdown signals.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve()
	}()

	fmt.Println("Signer ready — listening on UDS")

	select {
	case <-ctx.Done():
		fmt.Println("Signer shutting down gracefully...")
		session.Destroy()
		srv.GracefulStop()
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "signer server error: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("Signer stopped")
}
