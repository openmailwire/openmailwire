package config

import "testing"

func TestEnvSize(t *testing.T) {
	for in, want := range map[string]int64{
		"": DefaultInboundMaxSize, "25MB": 25 << 20, "25 mb": 25 << 20, "10MiB": 10 << 20, "512KB": 512 << 10,
		"1G": 1 << 30, "1048576": 1 << 20, "100B": 100,
	} {
		t.Setenv("OMW_TEST_SIZE", in)
		if got, err := envSize("OMW_TEST_SIZE", DefaultInboundMaxSize); err != nil || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "-1MB", "MB", "1.5MB", "lots", "99999999TB"} {
		t.Setenv("OMW_TEST_SIZE", bad)
		if _, err := envSize("OMW_TEST_SIZE", 1); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadInbound(t *testing.T) {
	t.Setenv("OMW_CONTROL_URL", "https://mail.example.com/")
	t.Setenv("OMW_WORKER_TOKEN", "omwk_x")
	t.Setenv("OMW_TLS_CERT", "/c.pem")
	t.Setenv("OMW_TLS_KEY", "")
	if _, err := LoadInbound(); err == nil {
		t.Fatal("a certificate without a key must be refused")
	}
	t.Setenv("OMW_TLS_KEY", "/k.pem")
	in, err := LoadInbound()
	if err != nil || in.ControlURL != "https://mail.example.com" || in.Listen != ":25" || in.MaxSize != DefaultInboundMaxSize {
		t.Fatalf("%+v %v", in, err)
	}
}

func TestDeliverabilitySettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OMW_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	c, err := LoadControl()
	if err != nil || !c.Warmup || c.ProviderRules == nil || c.ProviderRules.Provider("google") == nil {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	t.Setenv("OMW_WARMUP", "false")
	t.Setenv("OMW_PROVIDER_RULES", "/nonexistent/rules.json")
	if _, err := LoadControl(); err == nil {
		t.Fatal("a missing rules file must be refused")
	}
	t.Setenv("OMW_PROVIDER_RULES", "")
	if c, err := LoadControl(); err != nil || c.Warmup {
		t.Fatalf("OMW_WARMUP=false: %+v %v", c.Warmup, err)
	}

	t.Setenv("OMW_CONTROL_URL", "https://mail.example.com")
	t.Setenv("OMW_WORKER_TOKEN", "omwk_x")
	w, err := LoadWorker()
	if err != nil || w.SkipSelfCheck || w.SelfCheckSMTP != "" {
		t.Fatalf("worker defaults: %+v %v", w, err)
	}
	t.Setenv("OMW_SKIP_SELF_CHECK", "true")
	t.Setenv("OMW_SELF_CHECK_SMTP", "mx.example.net:25")
	if w, err := LoadWorker(); err != nil || !w.SkipSelfCheck || w.SelfCheckSMTP != "mx.example.net:25" {
		t.Fatalf("worker settings: %+v %v", w, err)
	}
	t.Setenv("OMW_SELF_CHECK_SMTP", "mx.example.net")
	if _, err := LoadWorker(); err == nil {
		t.Fatal("OMW_SELF_CHECK_SMTP without a port must be refused")
	}
}
