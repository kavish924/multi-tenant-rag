// Command worker runs background jobs. The job loop lands with spec 004; every
// job's database work will run inside storage.Store.WithTenant(job.TenantID)
// (TEN-REQ-009).
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("worker: not implemented yet (spec 004)")
}
