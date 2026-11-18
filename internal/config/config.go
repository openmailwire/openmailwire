// Package config reads settings from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Control configures the control plane (openmailwire serve).
type Control struct {
	DatabaseURL       string
	Listen            string
	SecretKey         []byte
	ReturnPathLabel   string
	SPFInclude        string
	BounceMX          string
	AllowUnverified   bool
	Retention         time.Duration
	DefaultExpiry     time.Duration
	LeaseDuration     time.Duration
	DomainConcurrency int
	DNSServer         string
}

// DatabaseURL returns DATABASE_URL or an error when it is unset.
func DatabaseURL() (string, error) {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		return "", errors.New("DATABASE_URL is not set")
	}
	return u, nil
}

func LoadControl() (Control, error) {
	c := Control{
		Listen:          env("OMW_LISTEN", ":8080"),
		ReturnPathLabel: env("OMW_RETURN_PATH_LABEL", "bounce"),
		SPFInclude:      os.Getenv("OMW_SPF_INCLUDE"),
		BounceMX:        os.Getenv("OMW_BOUNCE_MX"),
		DNSServer:       os.Getenv("OMW_DNS_SERVER"),
	}
	var err error
	if c.DatabaseURL, err = DatabaseURL(); err != nil {
		return c, err
	}
	raw := os.Getenv("OMW_SECRET_KEY")
	if raw == "" {
		return c, errors.New("OMW_SECRET_KEY is not set; generate one with `openmailwire keygen`")
	}
	if c.SecretKey, err = base64.StdEncoding.DecodeString(raw); err != nil || len(c.SecretKey) != 32 {
		return c, errors.New("OMW_SECRET_KEY must be 32 bytes, base64 encoded; generate one with `openmailwire keygen`")
	}
	if c.AllowUnverified, err = envBool("OMW_ALLOW_UNVERIFIED", false); err != nil {
		return c, err
	}
	if c.Retention, err = envDuration("OMW_RETENTION", 72*time.Hour); err != nil {
		return c, err
	}
	if c.DefaultExpiry, err = envDuration("OMW_DEFAULT_EXPIRY", 48*time.Hour); err != nil {
		return c, err
	}
	if c.LeaseDuration, err = envDuration("OMW_LEASE", 15*time.Minute); err != nil {
		return c, err
	}
	if c.DomainConcurrency, err = envInt("OMW_DOMAIN_CONCURRENCY", 10); err != nil {
		return c, err
	}
	return c, nil
}

// Worker configures a delivery worker (openmailwire worker).
type Worker struct {
	ControlURL   string
	Token        string
	EHLO         string
	SourceIP     string
	PublicIP     string
	SMTPOverride string
	StateDir     string
	Concurrency  int
	IPv4Only     bool
	PollInterval time.Duration
	JobTimeout   time.Duration
}

func LoadWorker() (Worker, error) {
	host, _ := os.Hostname()
	w := Worker{
		ControlURL:   strings.TrimRight(os.Getenv("OMW_CONTROL_URL"), "/"),
		Token:        os.Getenv("OMW_WORKER_TOKEN"),
		EHLO:         env("OMW_EHLO", host),
		SourceIP:     os.Getenv("OMW_SOURCE_IP"),
		PublicIP:     os.Getenv("OMW_PUBLIC_IP"),
		SMTPOverride: os.Getenv("OMW_SMTP_OVERRIDE"),
		StateDir:     env("OMW_STATE_DIR", "./openmailwire-worker-state"),
	}
	if w.PublicIP == "" {
		w.PublicIP = w.SourceIP
	}
	if w.ControlURL == "" {
		return w, errors.New("OMW_CONTROL_URL is not set")
	}
	if !strings.HasPrefix(w.Token, "omwk_") {
		return w, errors.New("OMW_WORKER_TOKEN is not set; create one with `openmailwire workers add <name>`")
	}
	var err error
	if w.Concurrency, err = envInt("OMW_CONCURRENCY", 20); err != nil {
		return w, err
	}
	if w.IPv4Only, err = envBool("OMW_IPV4_ONLY", true); err != nil {
		return w, err
	}
	if w.PollInterval, err = envDuration("OMW_POLL_INTERVAL", 2*time.Second); err != nil {
		return w, err
	}
	if w.JobTimeout, err = envDuration("OMW_JOB_TIMEOUT", 10*time.Minute); err != nil {
		return w, err
	}
	return w, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def, fmt.Errorf("%s must be a positive duration like 30m or 48h", key)
	}
	return d, nil
}
