// Command silo-plugin-filmography is a Silo plugin that serves full actor
// filmographies from TMDB, marking the titles that are in the library.
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
		Name:       "filmography",
		Level:      hclog.Info,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	app := newApp(logger)

	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		HttpRoutes: &routeServer{app: app},
	}, sdkruntime.WithConfigure(func(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
		app.configure(entries)
		return nil
	}))
}
