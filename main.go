// Command silo-plugin-wisp is a thin Silo request_router.v1 plugin that
// delegates all fulfillment to a Wisp server's HTTP API. It holds no business
// logic: every request is handed to Wisp (POST /api/add) and Wisp owns
// resolution, quality pinning, and status. Keeping the logic in Wisp's stable
// HTTP API means Silo SDK churn only ever touches this shim.
package main

import (
	_ "embed"
	"os"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/hashicorp/go-hclog"
)

//go:embed manifest.json
var manifestJSON []byte

// version is injected at build time via -ldflags "-X main.version=<version>".
// When empty (e.g. a bare `go build`), the version embedded in manifest.json is
// used unchanged: ServeManifest passes this straight to LoadWithChecksum, which
// only overrides the manifest version when it is non-empty.
var version string

// pluginLogger builds the plugin's log sink. go-plugin captures a plugin's
// stderr and, when a line parses as hclog JSON, re-emits it into Silo's own log
// at the line's level — so JSON on stderr is the supported way for a plugin to
// be heard. ServeManifest does not take a logger (ServeConfig.Logger is
// go-plugin's own internal sink), which is why this is wired into the router
// rather than into the serve config.
//
// Level is Info by default and overridable with WISP_PLUGIN_LOG_LEVEL, so a
// quiet plugin can be made loud without a rebuild.
func pluginLogger() hclog.Logger {
	level := hclog.LevelFromString(os.Getenv("WISP_PLUGIN_LOG_LEVEL"))
	if level == hclog.NoLevel {
		level = hclog.Info
	}
	return hclog.New(&hclog.LoggerOptions{
		Name:       "wisp",
		Level:      level,
		Output:     os.Stderr,
		JSONFormat: true,
	})
}

func main() {
	// ServeManifest is the SDK's canonical bootstrap. It performs the same
	// LoadWithChecksum(manifestJSON, version) + panic-on-error + Serve that this
	// plugin previously hand-rolled, but installs a Runtime server that also
	// answers Configure as a no-op. The hand-rolled runtimeServer implemented only
	// GetManifest, so Configure fell through to UnimplementedRuntimeServer and
	// returned codes.Unimplemented — if Silo calls Configure during install or
	// enable and treats a non-OK response as activation failure, the capability
	// would never be scheduled and CheckStatus would never be called.
	sdkruntime.ServeManifest(manifestJSON, version, sdkruntime.CapabilityServers{
		RequestRouter: newRouter(pluginLogger()),
	})
}
