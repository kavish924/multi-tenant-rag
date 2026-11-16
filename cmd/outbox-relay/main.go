// Command outbox-relay publishes outbox rows. It lands with spec 006.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("outbox-relay: not implemented yet (spec 006)")
}
