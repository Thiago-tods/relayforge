// Command delivery-worker is the RelayForge webhook delivery worker.
package main

import (
	"os"

	"github.com/Thiago-tods/relayforge/internal/platform/runtime"
)

func main() {
	os.Exit(runtime.Run("delivery-worker", ":8082", runtime.Options{RequireDatabase: false}))
}
