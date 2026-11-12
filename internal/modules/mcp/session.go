package mcp

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// CallAsSession runs a vault tool (search, browse, read, recent, memory_save,
// memory_context, ...) on tenant as the first-party session of user: Swift's
// MCPService.call with MCPCaller.session(userID:), which is how the Quick
// Assistant's runner calls it. The caller has already resolved tenant for
// user with the access the tool needs; nothing here checks it.
func (s *Service) CallAsSession(ctx context.Context, name string, args map[string]any, tenant, user uuid.UUID) (map[string]any, error) {
	return s.call(ctx, name, args, tenant, caller{userID: user})
}

// ToolErrorMessage reports the message of an MCPError (invalid params,
// unknown tool) a tool returned, which callers show as Swift's
// error.body.message.
func ToolErrorMessage(err error) (string, bool) {
	var e *rpcError
	if errors.As(err, &e) {
		return e.Message, true
	}
	return "", false
}

// PersonalResult is MCPService.toolResult(fromPersonalJSON:): a devicetools
// result with isError and message added when it failed.
func PersonalResult(raw string) map[string]any { return personalResult(raw) }
