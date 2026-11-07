// Command silo-plugin-email-alerts is a Silo plugin that emails new episodes
// and movies to a list of recipients using editable templates.
package main

import (
	"context"
	_ "embed"
	"os"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// version is stamped at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	logger := hclog.New(&hclog.LoggerOptions{
		Name:       "email-alerts",
		Level:      hclog.Info,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	app := newApp(logger)

	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		EventConsumer: &eventServer{app: app},
		ScheduledTask: &taskServer{app: app},
		HttpRoutes:    &routeServer{app: app},
	}, sdkruntime.WithConfigure(func(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
		app.configure(entries)
		return nil
	}))
}
