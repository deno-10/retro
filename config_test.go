package main

import (
	"strings"
	"testing"
)

func TestPaaSConfigFailsClosed(t *testing.T) {
	env := map[string]string{"RETRO_PROFILE": "paas"}
	get := func(k string) string { return env[k] }
	c, err := configFromEnv(get)
	if err != nil || c.Addr != ":8890" || c.Storage != "postgres" || !c.JSONLog || c.Options.AuthMode != "baas" {
		t.Fatalf("профиль PaaS: %+v %v", c, err)
	}
	if c.validate() == nil {
		t.Fatal("неполная конфигурация принята")
	}
	env["RETRO_DATABASE_URL"] = "postgres://test:secret@localhost/retro"
	env["RETRO_BAAS_SPACE"] = "retro-test"
	env["RETRO_TRUSTED_PROXY"] = "127.0.0.1/32"
	env["RETRO_PUBLIC_ORIGIN"] = "https://retro.example.test"
	c, err = configFromEnv(get)
	if err != nil || c.validate() != nil {
		t.Fatalf("корректная конфигурация: %v %v", err, c.validate())
	}
	for key, value := range map[string]string{"RETRO_STORAGE": "sqlite", "RETRO_AUTH_MODE": "local", "RETRO_TRUSTED_PROXY": "0.0.0.0/0", "RETRO_PUBLIC_ORIGIN": "https://retro.test/path", "RETRO_BASE_PATH": "/../api", "RETRO_SSE_TOTAL": "-1"} {
		t.Run(key, func(t *testing.T) {
			old := env[key]
			env[key] = value
			defer func() { env[key] = old }()
			bad, err := configFromEnv(get)
			if err == nil {
				err = bad.validate()
			}
			if err == nil {
				t.Fatal("небезопасная конфигурация принята")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("секрет попал в ошибку")
			}
		})
	}
}

func TestLocalConfigUnchanged(t *testing.T) {
	c, err := configFromEnv(func(string) string { return "" })
	if err != nil || c.validate() != nil || c.Addr != ":8080" || c.Data != "data/retro.db" || c.Storage != "sqlite" {
		t.Fatalf("локальные значения изменились: %+v %v", c, err)
	}
}
