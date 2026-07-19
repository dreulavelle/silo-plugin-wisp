// Command silo-plugin-wisp is a thin Silo request_router.v1 plugin that
// delegates all fulfillment to a Wisp server's HTTP API. It holds no business
// logic: every request is handed to Wisp (POST /api/add) and Wisp owns
// resolution, quality pinning, and status. Keeping the logic in Wisp's stable
// HTTP API means Silo SDK churn only ever touches this shim.
package main

import (
	_ "embed"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// version is injected at build time via -ldflags "-X main.version=<version>".
// When empty (e.g. a bare `go build`), the version embedded in manifest.json is
// used unchanged: ServeManifest passes this straight to LoadWithChecksum, which
// only overrides the manifest version when it is non-empty.
var version string

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
		RequestRouter: newRouter(),
	})
}
