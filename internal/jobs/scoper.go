package jobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// StoreScoper binds handlers' database work to the job's tenant through
// Store.WithTenant.
func StoreScoper(s *storage.Store) TenantScoper {
	return func(tenantID uuid.UUID) TenantTx {
		return func(ctx context.Context, fn func(pgx.Tx) error) error {
			return s.WithTenant(ctx, tenantID, fn)
		}
	}
}
