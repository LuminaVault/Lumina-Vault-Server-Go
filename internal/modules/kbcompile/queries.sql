-- name: UserTier :one
SELECT tier FROM users WHERE id = $1;
