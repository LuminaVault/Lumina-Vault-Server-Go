-- AgentsService, AgentRoomsController and AgentRoomOrchestrator (Sources/App/Agents).
-- Timestamps are written by the caller, as Fluent's @Timestamp writes Date().

-- name: CentralSessions :many
-- AgentsService.centralSessions, verbatim.
SELECT c.id, c.title, c.created_at, c.updated_at, COUNT(m.id)::int AS message_count
FROM conversations c
LEFT JOIN conversation_messages m ON m.conversation_id = c.id
WHERE c.tenant_id = sqlc.arg(tenant_id)
GROUP BY c.id
ORDER BY c.updated_at DESC NULLS LAST
LIMIT sqlc.arg(row_limit);

-- name: ConversationOwned :one
SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1 AND tenant_id = $2);

-- name: ConversationMessages :many
SELECT role, content, created_at FROM conversation_messages
WHERE conversation_id = $1
ORDER BY created_at ASC;

-- name: Personas :many
-- AgentRoomsController.candidateList: sort(isDefault, .descending), sort(label).
SELECT slug, label FROM user_hermes_profiles
WHERE tenant_id = $1
ORDER BY is_default DESC, label ASC;

-- name: HermesProfileID :one
SELECT hermes_profile_id FROM hermes_profiles WHERE tenant_id = $1 LIMIT 1;

-- name: ListRooms :many
SELECT id, tenant_id, title, token_budget, spent_tokens, stop_requested_at, created_at, updated_at
FROM agent_rooms
WHERE tenant_id = $1
ORDER BY updated_at DESC
LIMIT 100;

-- name: OwnedRoom :one
SELECT id, tenant_id, title, token_budget, spent_tokens, stop_requested_at, created_at, updated_at
FROM agent_rooms
WHERE tenant_id = $1 AND id = $2
LIMIT 1;

-- name: FindRoom :one
SELECT id, tenant_id, title, token_budget, spent_tokens, stop_requested_at, created_at, updated_at
FROM agent_rooms
WHERE id = $1;

-- name: InsertRoom :exec
INSERT INTO agent_rooms (id, tenant_id, title, token_budget, spent_tokens, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertMember :exec
INSERT INTO agent_room_members (id, room_id, instance_id, profile, handle, display_name, respond_mode, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: RoomMembers :many
SELECT id, room_id, instance_id, profile, handle, display_name, respond_mode, created_at
FROM agent_room_members
WHERE room_id = $1
ORDER BY created_at ASC;

-- name: DeleteRoom :exec
DELETE FROM agent_rooms WHERE id = $1;

-- name: SetStopRequested :exec
UPDATE agent_rooms SET stop_requested_at = $2, updated_at = $3 WHERE id = $1;

-- name: SetSpentTokens :exec
UPDATE agent_rooms SET spent_tokens = $2, updated_at = $3 WHERE id = $1;

-- name: LatestMessages :many
-- AgentRoomsController.detail: the latest page, newest first; the caller reverses it.
SELECT id, room_id, author_kind, member_id, body, tokens, created_at
FROM agent_room_messages
WHERE room_id = $1
ORDER BY created_at DESC
LIMIT 200;

-- name: InsertMessage :exec
INSERT INTO agent_room_messages (id, room_id, author_kind, member_id, body, tokens, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: LastOwnReply :many
-- AgentRoomOrchestrator.newMessages: the member's latest agent reply, if any.
SELECT created_at FROM agent_room_messages
WHERE room_id = $1 AND member_id = $2 AND author_kind = 'agent'
ORDER BY created_at DESC
LIMIT 1;

-- name: MessagesAfter :many
-- Everything after since, newest first, at most 20; the caller reverses it.
SELECT author_kind, member_id, body FROM agent_room_messages
WHERE room_id = sqlc.arg(room_id) AND created_at > sqlc.arg(since)
ORDER BY created_at DESC
LIMIT 20;

-- name: RecentMessages :many
SELECT author_kind, member_id, body FROM agent_room_messages
WHERE room_id = $1
ORDER BY created_at DESC
LIMIT 20;
