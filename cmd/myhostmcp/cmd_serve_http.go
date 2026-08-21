//go:build !remote_only

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"myhostmcp/internal/httpauth"
	"myhostmcp/internal/httpconfig"
	"myhostmcp/internal/httpserver"
)

// serveHTTPCmd runs an HTTPS MCP endpoint (streamable HTTP) authenticated with
// a root-managed token auth file (HTTP Basic and Bearer).
func serveHTTPCmd(args []string) {
	fs := flag.NewFlagSet("serve-http", flag.ExitOnError)
	configPath := fs.String("config", "", "path to HTTP server config (default: /etc/myhostmcp/http-server.yaml)")
	_ = fs.Parse(args)

	cfg, err := httpconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp serve-http: config error:", err)
		os.Exit(1)
	}
	authCfg, err := httpauth.Load(cfg.AuthConfigPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp serve-http: auth config error:", err)
		os.Exit(1)
	}
	srv, err := httpserver.New(cfg, authCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "myhostmcp serve-http: init error:", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      srv.Handler(),
		ReadTimeout:  time.Duration(cfg.ReadTimeout),
		WriteTimeout: time.Duration(cfg.WriteTimeout),
		IdleTimeout:  time.Duration(cfg.IdleTimeout),
	}

	ctx, cancel := signalContext()
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "myhostmcp serve-http:", err)
			os.Exit(1)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	srv.Close()
}
