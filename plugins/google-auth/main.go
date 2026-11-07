// Command silo-plugin-google-auth lets people sign in to Silo with Google.
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

//go:embed assets/google.svg
var googleSVG []byte

var version = ""

type routeServer struct {
	pluginv1.UnimplementedHttpRoutesServer
}

func (routeServer) Handle(_ context.Context, in *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if in.GetMethod() == "GET" && in.GetPath() == "/assets/google.svg" {
		return &pluginv1.HandleHTTPResponse{StatusCode: 200, Body: googleSVG, Headers: map[string]string{
			"Content-Type": "image/svg+xml", "Cache-Control": "public, max-age=86400"}}, nil
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: 404, Body: []byte("not found")}, nil
}

func main() {
	logger := hclog.New(&hclog.LoggerOptions{Name: "google-auth", Level: hclog.Info, Output: os.Stderr, JSONFormat: true})
	p := newProvider(logger)
	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		AuthProvider: p,
		HttpRoutes:   routeServer{},
	}, sdkruntime.WithAuthProviderChecks(p),
		sdkruntime.WithConfigure(func(_ context.Context, entries []*pluginv1.ConfigEntry) error {
			p.configure(entries)
			return nil
		}))
}
