// Command api is the RelayForge HTTP API process.
package main

import (
	"os"

	"github.com/Thiago-tods/relayforge/internal/platform/runtime"
)

func main() {
	os.Exit(runtime.Run("api", ":8080", runtime.Options{RequireDatabase: true}))
}
