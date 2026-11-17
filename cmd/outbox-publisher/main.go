// Command outbox-publisher is the RelayForge outbox publisher worker.
package main

import (
	"os"

	"github.com/relayforge/relayforge/internal/platform/runtime"
)

func main() {
	os.Exit(runtime.Run("outbox-publisher", ":8081"))
}
