package main

import (
	"fmt"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Config is the admin form configuration Silo delivers through Configure.
// Templates, recipients and schedules live in instance state instead, so
// editing them does not restart the plugin.
type Config struct {
	APIKey     string
	ProfileID  string
	BaseURL    string
	PublicURL  string
	ServerName string
	SMTP       SMTPConfig
}

type SMTPConfig struct {
	Host        string
	Port        int
	Security    string // starttls | tls | none
	Username    string
	Password    string
	FromAddress string
	FromName    string
	ReplyTo     string
}

func parseConfig(entries []*pluginv1.ConfigEntry) Config {
	cfg := Config{SMTP: SMTPConfig{Port: 587, Security: "starttls"}}
	for _, entry := range entries {
		if entry == nil || entry.GetValue() == nil {
			continue
		}
		v := entry.GetValue().AsMap()
		switch entry.GetKey() {
		case "silo":
			cfg.APIKey = str(v["api_key"])
			cfg.ProfileID = str(v["profile_id"])
			cfg.BaseURL = strings.TrimRight(str(v["base_url"]), "/")
			cfg.PublicURL = strings.TrimRight(str(v["public_url"]), "/")
			cfg.ServerName = str(v["server_name"])
		case "smtp":
			cfg.SMTP.Host = str(v["host"])
			if port := num(v["port"]); port > 0 && port <= 65535 {
				cfg.SMTP.Port = port
			}
			if sec := str(v["security"]); sec != "" {
				cfg.SMTP.Security = sec
			}
			cfg.SMTP.Username = str(v["username"])
			cfg.SMTP.Password = str(v["password"])
			cfg.SMTP.FromAddress = str(v["from_address"])
			cfg.SMTP.FromName = str(v["from_name"])
			cfg.SMTP.ReplyTo = str(v["reply_to"])
		}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "Silo"
	}
	return cfg
}

// problems lists what stops the plugin from sending, for the admin page.
func (c Config) problems() []string {
	var out []string
	if c.APIKey == "" {
		out = append(out, "No Silo API key: add one under the plugin's Silo connection settings.")
	}
	if c.SMTP.Host == "" {
		out = append(out, "No SMTP host: fill in the plugin's Outgoing mail settings.")
	}
	if c.SMTP.FromAddress == "" {
		out = append(out, "No From address: fill in the plugin's Outgoing mail settings.")
	}
	return out
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		if t != t || t > 1e9 || t < -1e9 { // NaN or absurd
			return 0
		}
		return int(t)
	case int:
		return t
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(t), "%d", &n)
		return n
	}
	return 0
}
