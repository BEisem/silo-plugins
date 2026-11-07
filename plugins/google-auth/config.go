package main

import (
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// config is the operator's Google settings, parsed from ConfigEntry values.
// Who may sign in and what role they get both come from Silo's own accounts,
// so there are no email lists here.
type config struct {
	ClientID     string
	ClientSecret string
	Prompt       string
	SiloAPIKey   string
}

// parseConfig reads the "google" entry. Missing or empty settings are fine:
// Configure must succeed on a fresh install.
func parseConfig(entries []*pluginv1.ConfigEntry) config {
	var c config
	for _, e := range entries {
		if e.GetKey() != "google" || e.GetValue() == nil {
			continue
		}
		m := e.GetValue().AsMap()
		str := func(k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
		c.ClientID = str("client_id")
		c.ClientSecret = str("client_secret")
		c.Prompt = str("prompt")
		c.SiloAPIKey = str("silo_api_key")
	}
	return c
}
