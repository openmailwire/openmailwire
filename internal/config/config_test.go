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
