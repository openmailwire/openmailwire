package config

import (
	"slices"
	"testing"
)

func TestFBLTrustedDomains(t *testing.T) {
	t.Setenv("OMW_FBL_TRUSTED_DOMAINS", "")
	if got, _ := domainList("OMW_FBL_TRUSTED_DOMAINS", DefaultFBLTrustedDomains); !slices.Equal(got, DefaultFBLTrustedDomains) {
		t.Fatalf("default: %v", got)
	}
	t.Setenv("OMW_FBL_TRUSTED_DOMAINS", "Yahoo.com, .hotmail.com mail.ru.")
	if got, err := domainList("OMW_FBL_TRUSTED_DOMAINS", nil); err != nil || !slices.Equal(got, []string{"yahoo.com", "hotmail.com", "mail.ru"}) {
		t.Fatalf("list: %v %v", got, err)
	}
	t.Setenv("OMW_FBL_TRUSTED_DOMAINS", "none")
	if got, _ := domainList("OMW_FBL_TRUSTED_DOMAINS", DefaultFBLTrustedDomains); len(got) != 0 {
		t.Fatalf("none: %v", got)
	}
	t.Setenv("OMW_FBL_TRUSTED_DOMAINS", "fbl@yahoo.com")
	if _, err := domainList("OMW_FBL_TRUSTED_DOMAINS", nil); err == nil {
		t.Fatal("an address accepted as a domain")
	}
}
