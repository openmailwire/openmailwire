package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/submission"
)

// runSubmission serves SMTP submission for apps that cannot call the HTTP
// API. It keeps no state: every message goes to the control plane.
//
// --dev-tls makes a self-signed certificate for local development when
// OMW_TLS_CERT is not set: clients must then skip certificate checks.
func runSubmission(ctx context.Context, log *slog.Logger, args []string) error {
	switch {
	case len(args) == 1 && args[0] == "--dev-tls":
		if os.Getenv("OMW_TLS_CERT") != "" || os.Getenv("OMW_TLS_KEY") != "" {
			return errors.New("--dev-tls makes its own certificate; unset OMW_TLS_CERT and OMW_TLS_KEY, or drop --dev-tls")
		}
		dir, err := os.MkdirTemp("", "openmailwire-dev-tls-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		host, _ := os.Hostname()
		cert, key, err := writeDevCert(dir, os.Getenv("OMW_SUBMISSION_HOSTNAME"), host)
		if err != nil {
			return err
		}
		os.Setenv("OMW_TLS_CERT", cert)
		os.Setenv("OMW_TLS_KEY", key)
		log.Warn("--dev-tls: serving a self-signed certificate; clients must skip certificate verification. Development only, never in production")
	case len(args) != 0:
		return errors.New("usage: openmailwire submission [--dev-tls]")
	}
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
