package main

import (
	"fmt"
	"net/netip"
	"strconv"

	"retro/internal/ollama"
	"retro/internal/server"
)

type runtimeConfig struct {
	Profile, Addr, Data, Storage, DatabaseURL string
	JSONLog                                   bool
	Options                                   server.Options
}

// Профиль paas намеренно не имеет запасного SQLite/анонимного входа.
func configFromEnv(get func(string) string) (runtimeConfig, error) {
	c := runtimeConfig{Profile: get("RETRO_PROFILE"), Addr: ":8080", Data: "data/retro.db", Storage: "sqlite", Options: server.DefaultOptions()}
	if c.Profile != "" && c.Profile != "local" && c.Profile != "paas" {
		return c, fmt.Errorf("RETRO_PROFILE: допустимы local и paas")
	}
	if c.Profile == "paas" {
		c.Addr, c.Storage, c.JSONLog, c.Options.AuthMode = ":8890", "postgres", true, "baas"
	}
	for key, target := range map[string]*string{
		"RETRO_ADDR": &c.Addr, "RETRO_DATA": &c.Data, "RETRO_STORAGE": &c.Storage,
		"RETRO_AUTH_MODE": &c.Options.AuthMode, "RETRO_BAAS_SPACE": &c.Options.BaaSSpace,
		"RETRO_PUBLIC_ORIGIN": &c.Options.PublicOrigin, "RETRO_BASE_PATH": &c.Options.BasePath,
	} {
		if v := get(key); v != "" {
			*target = v
		}
	}
	c.DatabaseURL = get("RETRO_DATABASE_URL")
	localModel, err := ollama.New(get("RETRO_OLLAMA_MODEL"), get("RETRO_OLLAMA_ADDR"))
	if err != nil {
		return c, err
	}
	c.Options.Ollama = localModel
	if proxy := get("RETRO_TRUSTED_PROXY"); proxy != "" {
		p, err := netip.ParsePrefix(proxy)
		if err != nil {
			return c, fmt.Errorf("неверный RETRO_TRUSTED_PROXY")
		}
		c.Options.TrustedProxy = p
	}
	if v := get("RETRO_LOG_JSON"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("неверный RETRO_LOG_JSON")
		}
		c.JSONLog = b
	}
	for key, target := range map[string]*int{"RETRO_SSE_PER_IP": &c.Options.SSEPerIP, "RETRO_SSE_PER_BOARD": &c.Options.SSEPerBoard, "RETRO_SSE_TOTAL": &c.Options.SSETotal} {
		if v := get(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("%s должен быть положительным целым числом", key)
			}
			*target = n
		}
	}
	return c, nil
}

func (c runtimeConfig) validate() error {
	if c.Storage != "sqlite" && c.Storage != "postgres" {
		return fmt.Errorf("RETRO_STORAGE: допустимы sqlite и postgres")
	}
	if c.Storage == "postgres" && c.DatabaseURL == "" {
		return fmt.Errorf("RETRO_DATABASE_URL обязателен для PostgreSQL")
	}
	if c.Profile == "paas" && (c.Storage != "postgres" || c.Options.AuthMode != "baas") {
		return fmt.Errorf("профиль paas требует PostgreSQL и BaaS")
	}
	return c.Options.ValidatePlatform()
}
