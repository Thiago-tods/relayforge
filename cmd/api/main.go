// Command api is the RelayForge HTTP API process.
package main

import (
	"os"

	"github.com/relayforge/relayforge/internal/platform/runtime"
)

func main() {
	os.Exit(runtime.Run("api", ":8080"))
}
