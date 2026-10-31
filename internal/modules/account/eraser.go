package account

import (
	"context"
	"lumina/internal/ids"
	"lumina/internal/modules/account/db"
	"strings"

	"github.com/google/uuid"
)

// TenantDataEraser's lists. The schema test (module_test.go) fails when a
// table with a tenant_id or user_id column neither cascades from users nor
// appears in one of them, as Swift's AccountDeletionTests does.
var (
	// ErasedTables are deleted outright, WHERE tenant_id = <user id>.
	ErasedTables = []string{
		"agent_rooms",
		"device_command_queue",
		"idempotency_keys",
		"embedding_usage",
		"retrieval_telemetry_events",
		"retrieval_leak_reports",
	}
	// AnonymizedTables keep their rows with the personal identifier removed.
	AnonymizedTables = []string{"billing_event_logs", "vault_activity_events"}
	// HandledElsewhere: provider_failover_events.tenant_id is ON DELETE SET NULL.
	HandledElsewhere = []string{"provider_failover_events"}
)

// DeletedActorName replaces actor_name on activity the user left in vaults
// that outlive them.
const DeletedActorName = "Deleted user"

// Erase is TenantDataEraser.erase: the tenant rows no ON DELETE CASCADE from
// users reaches, in Swift's order. Run it in the users-delete transaction.
func Erase(ctx context.Context, q *db.Queries, tenant uuid.UUID) error {
	for _, erase := range []func(context.Context, uuid.UUID) error{
		q.EraseAgentRooms,
		q.EraseDeviceCommandQueue,
		q.EraseIdempotencyKeys,
		q.EraseEmbeddingUsage,
		q.EraseRetrievalTelemetryEvents,
		q.EraseRetrievalLeakReports,
	} {
		if err := erase(ctx, tenant); err != nil {
			return err
		}
	}
	lower := strings.ToLower(ids.Upper(tenant))
	if err := q.EraseSpendBuckets(ctx, db.EraseSpendBucketsParams{TenantKey: "tenant:" + lower, FreelaneKey: "freelane:tenant:" + lower}); err != nil {
		return err
	}
	// Billing records are a legal retention obligation: kept, user_id nulled.
	if err := q.AnonymizeBillingEvents(ctx, tenant); err != nil {
		return err
	}
	return q.AnonymizeVaultActivity(ctx, db.AnonymizeVaultActivityParams{ActorName: DeletedActorName, ActorUserID: tenant})
}
