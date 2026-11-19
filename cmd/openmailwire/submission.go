package main

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/submission"
)

// runSubmission serves SMTP submission for apps that cannot call the HTTP
// API. It keeps no state: every message goes to the control plane.
func runSubmission(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadSubmission()
	if err != nil {
		return err
	}
	tlsCfg, err := submission.LoadTLS(cfg.TLSCert, cfg.TLSKey, log)
	if err != nil {
		return err
	}
	srv, err := submission.New(submission.Config{
		Hostname: cfg.Hostname, APIURL: cfg.APIURL, TLS: tlsCfg, MaxSize: cfg.MaxSize,
		MaxConns: cfg.MaxConns, MaxConnsPerIP: cfg.MaxConnsPerIP, Log: log, Version: version,
	})
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	go func() { errc <- srv.Serve(ln) }()
	if cfg.TLSListen != "" {
		tln, err := net.Listen("tcp", cfg.TLSListen)
		if err != nil {
			srv.Close()
			return err
		}
		go func() { errc <- srv.ServeTLS(tln) }()
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := srv.CheckAPI(cctx); err != nil {
		log.Warn("the control plane does not answer yet; logins get 454 and messages 451 until it does", "api", cfg.APIURL, "err", err)
	}
	cancel()
	log.Info("submission listening", "addr", cfg.Listen, "tls_addr", cfg.TLSListen, "hostname", cfg.Hostname,
		"api", cfg.APIURL, "max_size", cfg.MaxSize, "version", version)

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
		return nil
	case err := <-errc:
		srv.Close()
		return err
	}
}
