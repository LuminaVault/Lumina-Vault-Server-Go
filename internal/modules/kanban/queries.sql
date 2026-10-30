-- Kanban (Swift: Sources/App/Kanban/KanbanService.swift). Each query is the
-- statement Fluent issues for the Swift call named in its comment, with the
-- same filters and ORDER BY (or none), because two of them feed list output.

-- name: InsertBoard :exec
-- createBoard: Fluent create sets created_at = updated_at.
INSERT INTO kanban_boards (id, tenant_id, title, version, created_at, updated_at)
VALUES ($1, $2, $3, 0, $4, $4);

-- name: SetBoardAuthors :exec
-- KanbanController.createBoard: b.createdByUserID = b.updatedByUserID = actor; b.update.
UPDATE kanban_boards SET created_by_user_id = $2, updated_by_user_id = $2, updated_at = $3 WHERE id = $1;

-- name: ListBoards :many
-- listBoards: no sort in Swift, so rows come back in heap order.
SELECT id, title, version, updated_at FROM kanban_boards
WHERE tenant_id = $1 AND archived_at IS NULL;

-- name: CountColumns :one
SELECT count(*) FROM kanban_columns WHERE board_id = $1;

-- name: CountCards :one
SELECT count(*) FROM kanban_cards WHERE board_id = $1;

-- name: DefaultBoard :one
-- defaultBoard: oldest live board.
SELECT id FROM kanban_boards
WHERE tenant_id = $1 AND archived_at IS NULL
ORDER BY created_at ASC
LIMIT 1;

-- name: GetBoard :one
SELECT id, tenant_id, title, version, archived_at, created_by_user_id, updated_by_user_id
FROM kanban_boards WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: ClaimVersion :one
-- claimMutation without an expected version.
UPDATE kanban_boards
SET version = version + 1, updated_at = NOW()
WHERE id = $1 AND tenant_id = $2
RETURNING version;

-- name: ClaimExpectedVersion :one
-- claimMutation with X-Board-Version.
UPDATE kanban_boards
SET version = version + 1, updated_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND version = $3
RETURNING version;

-- name: UpdateBoard :exec
-- patchBoard: title and/or archived_at (only when something was assigned).
UPDATE kanban_boards SET title = $2, archived_at = $3, updated_at = $4 WHERE id = $1;

-- name: DeleteBoard :exec
DELETE FROM kanban_boards WHERE id = $1;

-- name: LastColumnRank :one
SELECT rank FROM kanban_columns WHERE board_id = $1 ORDER BY rank DESC LIMIT 1;

-- name: InsertColumn :exec
INSERT INTO kanban_columns (id, tenant_id, board_id, title, rank, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6);

-- name: GetColumn :one
SELECT id, board_id, rank FROM kanban_columns WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: SiblingColumn :one
SELECT rank FROM kanban_columns WHERE id = $1 AND board_id = $2 LIMIT 1;

-- name: UpdateColumnTitle :exec
UPDATE kanban_columns SET title = $2, updated_at = $3 WHERE id = $1;

-- name: UpdateColumnRank :exec
UPDATE kanban_columns SET rank = $2, updated_at = $3 WHERE id = $1;

-- name: DeleteColumn :exec
DELETE FROM kanban_columns WHERE id = $1;

-- name: ListColumns :many
-- snapshot: columns by rank.
SELECT id, title, rank FROM kanban_columns WHERE board_id = $1 ORDER BY rank ASC;

-- name: LastCardRank :one
SELECT rank FROM kanban_cards WHERE column_id = $1 ORDER BY rank DESC LIMIT 1;

-- name: InsertCard :exec
INSERT INTO kanban_cards (id, tenant_id, board_id, column_id, title, body, priority, due_at, rank, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- name: SetCardAuthors :exec
UPDATE kanban_cards SET created_by_user_id = $2, updated_by_user_id = $2, updated_at = $3 WHERE id = $1;

-- name: GetCard :one
SELECT id, board_id, column_id, title, body, rank, priority, due_at, updated_at, extra, created_by_user_id, updated_by_user_id
FROM kanban_cards WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: SiblingCard :one
SELECT rank FROM kanban_cards WHERE id = $1 AND column_id = $2 LIMIT 1;

-- name: UpdateCard :exec
-- patchCard (only when a field was assigned).
UPDATE kanban_cards SET title = $2, body = $3, priority = $4, due_at = $5, updated_at = $6 WHERE id = $1;

-- name: MoveCard :exec
UPDATE kanban_cards SET column_id = $2, rank = $3, updated_at = $4 WHERE id = $1;

-- name: UpdateCardExtra :exec
-- promoteCard: card.extra = extra; card.update.
UPDATE kanban_cards SET extra = $2, updated_at = $3 WHERE id = $1;

-- name: DeleteCard :exec
DELETE FROM kanban_cards WHERE id = $1;

-- name: ListCards :many
-- snapshot: cards of one column by rank.
SELECT id, board_id, column_id, title, body, rank, priority, due_at, updated_at, extra, created_by_user_id, updated_by_user_id
FROM kanban_cards WHERE column_id = $1 ORDER BY rank ASC;

-- name: UpsertSkillState :exec
-- JobAuthoring.author: enable the vault skill (Skills/JobAuthoring.swift).
INSERT INTO skills_state (tenant_id, source, name, enabled, domain, space_id, run_at, origin_kind, origin_id)
VALUES ($1, 'vault', $2, TRUE, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, source, name) DO UPDATE
  SET enabled = TRUE, domain = EXCLUDED.domain, space_id = EXCLUDED.space_id, run_at = EXCLUDED.run_at,
      origin_kind = COALESCE(EXCLUDED.origin_kind, skills_state.origin_kind),
      origin_id = COALESCE(EXCLUDED.origin_id, skills_state.origin_id);
