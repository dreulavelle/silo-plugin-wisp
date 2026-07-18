// Command silo-plugin-wisp is a thin Silo request_router.v1 plugin that
// delegates all fulfillment to a Wisp server's HTTP API. It holds no business
// logic: every request is handed to Wisp (POST /api/add) and Wisp owns
// resolution, quality pinning, and status. Keeping the logic in Wisp's stable
// HTTP API means Silo SDK churn only ever touches this shim.
package main

import (
	"context"
	_ "embed"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
)

//go:embed manifest.json
var manifestJSON []byte

// version is injected at build time via -ldflags "-X main.version=<version>".
// When empty (e.g. a bare `go build`), the version embedded in manifest.json is
// used unchanged. LoadWithChecksum only overrides the manifest version when this
// is non-empty.
var version string

// runtimeServer serves the plugin manifest. It embeds runtimedefault.Server so
// the host's Runtime.BindHostBroker is handled for us.
type runtimeServer struct {
	runtimedefault.Server
	manifest *pluginv1.PluginManifest
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func main() {
	// LoadWithChecksum decodes the embedded manifest, overrides its version with
	// the build-time value (when set), stamps the running binary's sha256
	// checksum, and validates. The manifest declares its supported platforms
	// explicitly, so no host-arch fallback is needed.
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		panic(err)
	}

	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:       &runtimeServer{manifest: manifest},
			RequestRouter: newRouter(),
		},
	})
}
