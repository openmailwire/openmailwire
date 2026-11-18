package config

import (
	"strings"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1048576": 1 << 20, "14MB": 14 << 20, "512kb": 512 << 10, "10 M": 10 << 20, "2048B": 2048, "1GB": 1 << 30,
	} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1MB", "1.5MB", "ten"} {
		if _, err := parseByteSize(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestLoadSubmission(t *testing.T) {
	t.Setenv("OMW_TLS_CERT", "")
	t.Setenv("OMW_TLS_KEY", "")
	if _, err := LoadSubmission(); err == nil || !strings.Contains(err.Error(), "OMW_TLS_CERT") {
		t.Fatalf("without a certificate: %v", err)
	}
	t.Setenv("OMW_TLS_CERT", "/etc/cert.pem")
	t.Setenv("OMW_TLS_KEY", "/etc/key.pem")
	t.Setenv("OMW_API_URL", "https://mail.example.com/")
	t.Setenv("OMW_SUBMISSION_LISTEN", "")
	t.Setenv("OMW_SUBMISSION_MAX_SIZE", "")
	c, err := LoadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":587" || c.TLSListen != "" || c.APIURL != "https://mail.example.com" || c.MaxSize != SubmissionMaxSize {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("OMW_SUBMISSION_MAX_SIZE", "20MB")
	if _, err := LoadSubmission(); err == nil {
		t.Fatal("a size above what /emails/raw accepts must be refused")
	}
}
