package config

import (
	"strings"
	"testing"
)

func TestUnitFakePhoneConfigurationIsOptInAndNeverProd(t *testing.T) {
	for _, environment := range []string{"dev", "stage", "prod"} {
		vars := base()
		vars["ENV"], vars["PHONE_PROVIDER"] = environment, "fake"
		if environment == "dev" {
			vars["AUTH_MODE"] = "insecure-dev"
		}
		if environment == "stage" {
			vars["AUTH_STAGE_ISSUER"], vars["AUTH_STAGE_SIGNING_KEY_FILE"], vars["AUTH_STAGE_PUBLIC_KEYS_FILE"] = "stage-services", "/stage.pem", "/stage.jwks"
		}
		g, l := env(vars)
		cfg, err := load(g, noFiles, l)
		if environment == "prod" {
			if err == nil || !strings.Contains(err.Error(), "fake backend") {
				t.Fatal(cfg, err)
			}
		} else if err != nil || cfg.Zvonok.Provider != "fake" {
			t.Fatal(cfg.Zvonok, err)
		}
	}
	for _, c := range []ZvonokConfig{
		{Provider: "fake", PublicKey: "secret"},
		{Provider: "fake", CampaignID: "123"},
	} {
		if err := c.Validate(EnvDev); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("real credentials accepted or leaked", err)
		}
	}
	if err := (ZvonokConfig{Provider: "fake"}).Validate(Env("unknown")); err == nil {
		t.Fatal("unknown environment accepted")
	}
}
