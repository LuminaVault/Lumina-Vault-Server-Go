-- SOUL.md (Swift: Sources/App/Auth/SoulController.swift). The routes read only the
-- caller's username, which names the Hermes profile directory.

-- name: Username :one
-- Swift's jwtAuthenticator loads the User; SOULService uses user.username.
SELECT username FROM users WHERE id = $1;
