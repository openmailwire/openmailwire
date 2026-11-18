package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SubmissionMaxSize is the largest message the submission server accepts:
// POST /emails/raw takes at most 20 MB of JSON, and base64 adds a third.
const SubmissionMaxSize = 14 << 20

// Submission configures the SMTP submission server (openmailwire
// submission).
type Submission struct {
	Listen    string // STARTTLS, required before AUTH
	TLSListen string // implicit TLS; empty = off
	Hostname  string // announced in the greeting
	TLSCert   string
	TLSKey    string
	APIURL    string // the control plane
	MaxSize   int64
}

func LoadSubmission() (Submission, error) {
	host, _ := os.Hostname()
	c := Submission{
		Listen:    env("OMW_SUBMISSION_LISTEN", ":587"),
		TLSListen: os.Getenv("OMW_SUBMISSION_TLS_LISTEN"),
		Hostname:  env("OMW_SUBMISSION_HOSTNAME", host),
		TLSCert:   os.Getenv("OMW_TLS_CERT"),
		TLSKey:    os.Getenv("OMW_TLS_KEY"),
		APIURL:    strings.TrimRight(env("OMW_API_URL", "http://localhost:8080"), "/"),
		MaxSize:   SubmissionMaxSize,
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return c, errors.New("OMW_TLS_CERT and OMW_TLS_KEY are required: SMTP clients send their API key as a password, so it must travel over TLS")
	}
	if v := os.Getenv("OMW_SUBMISSION_MAX_SIZE"); v != "" {
		n, err := parseByteSize(v)
		if err != nil || n < 1024 || n > SubmissionMaxSize {
			return c, fmt.Errorf("OMW_SUBMISSION_MAX_SIZE must be a size like 10MB, between 1KB and 14MB")
		}
		c.MaxSize = n
	}
	return c, nil
}

// parseByteSize reads "14MB", "512KB", "1048576" and the like. KB, MB and
// GB are powers of 1024.
func parseByteSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(v), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
