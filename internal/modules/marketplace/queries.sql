-- Marketplace (Sources/App/Marketplace/MarketplaceService.swift), plus the
-- plugin_installs surface of PluginService.installMarketplace/upgradeMarketplace.

-- name: IsAdmin :one
SELECT is_admin FROM users WHERE id = $1;

-- name: Username :one
SELECT username FROM users WHERE id = $1;

-- name: FirstAdmin :one
SELECT id FROM users WHERE is_admin = TRUE LIMIT 1;

-- name: PublisherExistsByHandle :one
SELECT EXISTS (SELECT 1 FROM marketplace_publishers WHERE handle = $1) AS hit;

-- name: InsertPublisher :exec
INSERT INTO marketplace_publishers
  (id, owner_user_id, handle, display_name, bio, website_url, status, verified, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(handle), sqlc.arg(display_name), sqlc.narg(bio), sqlc.narg(website_url),
        sqlc.arg(status), sqlc.arg(verified), sqlc.arg(at), sqlc.arg(at));

-- name: PublisherByID :one
SELECT * FROM marketplace_publishers WHERE id = $1;

-- name: PublisherByOwner :one
SELECT * FROM marketplace_publishers WHERE owner_user_id = $1;

-- name: PendingPublishers :many
SELECT * FROM marketplace_publishers WHERE status = 'pending' ORDER BY created_at ASC;

-- name: SetPublisherStatus :one
UPDATE marketplace_publishers SET status = $2, verified = $3, updated_at = $4 WHERE id = $1 RETURNING *;

-- name: PublishedListings :many
SELECT * FROM marketplace_listings WHERE status = 'published' ORDER BY featured DESC, name ASC;

-- name: PublishedListingBySlug :one
SELECT * FROM marketplace_listings WHERE slug = $1 AND status = 'published';

-- name: ListingBySlug :one
SELECT * FROM marketplace_listings WHERE slug = $1;

-- name: ListingByID :one
SELECT * FROM marketplace_listings WHERE id = $1;

-- name: OwnedListing :one
SELECT * FROM marketplace_listings WHERE slug = $1 AND publisher_id = $2;

-- name: InsertListing :one
INSERT INTO marketplace_listings
  (id, publisher_id, slug, name, summary, description, category, icon_url, screenshots, status, featured, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(publisher_id), sqlc.arg(slug), sqlc.arg(name), sqlc.arg(summary), sqlc.arg(description), sqlc.arg(category),
        sqlc.narg(icon_url), sqlc.arg(screenshots), sqlc.arg(status), sqlc.arg(featured), sqlc.arg(at), sqlc.arg(at))
RETURNING *;

-- name: PublishListing :exec
UPDATE marketplace_listings SET status = $2, updated_at = $3 WHERE id = $1;

-- name: LatestApprovedVersion :one
SELECT * FROM marketplace_versions WHERE listing_id = $1 AND status = 'approved' ORDER BY published_at DESC LIMIT 1;

-- name: VersionByID :one
SELECT * FROM marketplace_versions WHERE id = $1;

-- name: ListingVersion :one
SELECT * FROM marketplace_versions WHERE id = $1 AND listing_id = $2;

-- name: ApprovedListingVersion :one
SELECT * FROM marketplace_versions WHERE id = $1 AND listing_id = $2 AND status = 'approved';

-- name: InsertVersion :one
INSERT INTO marketplace_versions
  (id, listing_id, version, status, runtime_kind, permissions, network_hosts, config_fields, changelog,
   artifact_key, artifact_sha256, artifact_signature, manifest_json, published_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(listing_id), sqlc.arg(version), sqlc.arg(status), sqlc.arg(runtime_kind), sqlc.arg(permissions),
        sqlc.arg(network_hosts), sqlc.arg(config_fields), sqlc.narg(changelog), sqlc.narg(artifact_key), sqlc.narg(artifact_sha256),
        sqlc.narg(artifact_signature), sqlc.narg(manifest_json), sqlc.narg(published_at), sqlc.arg(at), sqlc.arg(at))
RETURNING *;

-- name: SetVersionStatus :exec
UPDATE marketplace_versions SET status = $2, published_at = sqlc.narg(published_at), updated_at = $3 WHERE id = $1;

-- name: RatingsForListing :many
SELECT rating FROM marketplace_ratings WHERE listing_id = $1 AND moderation_status = 'published';

-- name: Reviews :many
SELECT r.id, r.rating, r.body, r.created_at, r.updated_at, u.username
FROM marketplace_ratings r JOIN users u ON u.id = r.user_id
WHERE r.listing_id = $1 AND r.moderation_status = 'published'
ORDER BY r.created_at DESC;

-- name: RatingByUser :one
SELECT * FROM marketplace_ratings WHERE listing_id = $1 AND user_id = $2;

-- name: InsertRating :one
INSERT INTO marketplace_ratings (id, listing_id, user_id, rating, body, moderation_status, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(listing_id), sqlc.arg(user_id), sqlc.arg(rating), sqlc.narg(body), 'published', sqlc.arg(at), sqlc.arg(at))
RETURNING *;

-- name: UpdateRating :one
UPDATE marketplace_ratings SET rating = sqlc.arg(rating), body = sqlc.narg(body), moderation_status = 'published', updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SubmissionByID :one
SELECT * FROM marketplace_submissions WHERE id = $1;

-- name: SubmissionByVersion :one
SELECT * FROM marketplace_submissions WHERE version_id = $1;

-- name: InsertSubmission :one
INSERT INTO marketplace_submissions
  (id, version_id, publisher_user_id, status, validation_errors, submitted_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(version_id), sqlc.arg(publisher_user_id), sqlc.arg(status), sqlc.arg(validation_errors),
        sqlc.narg(submitted_at), sqlc.arg(at), sqlc.arg(at))
RETURNING *;

-- name: ResubmitSubmission :one
UPDATE marketplace_submissions
SET publisher_user_id = sqlc.arg(publisher_user_id), status = sqlc.arg(status), validation_errors = sqlc.arg(validation_errors),
    submitted_at = sqlc.narg(submitted_at), review_note = NULL, reviewed_at = NULL, reviewed_by_user_id = NULL, updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: ReviewSubmission :one
UPDATE marketplace_submissions
SET status = sqlc.arg(status), review_note = sqlc.narg(review_note), reviewed_by_user_id = sqlc.arg(reviewed_by_user_id),
    reviewed_at = sqlc.arg(at), updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: AllSubmissions :many
SELECT s.*, l.slug FROM marketplace_submissions s
JOIN marketplace_versions v ON v.id = s.version_id
JOIN marketplace_listings l ON l.id = v.listing_id
ORDER BY s.created_at DESC;

-- name: PublisherSubmissions :many
SELECT s.*, l.slug FROM marketplace_submissions s
JOIN marketplace_versions v ON v.id = s.version_id
JOIN marketplace_listings l ON l.id = v.listing_id
WHERE s.publisher_user_id = $1
ORDER BY s.created_at DESC;

-- name: InstallCount :one
SELECT count(*) FROM plugin_installs WHERE plugin_slug = $1;

-- name: InstallBySlug :one
SELECT * FROM plugin_installs WHERE tenant_id = $1 AND plugin_slug = $2;

-- name: EnabledInstall :one
SELECT * FROM plugin_installs WHERE tenant_id = $1 AND plugin_slug = $2 AND status = 'enabled';

-- name: InsertInstall :one
INSERT INTO plugin_installs
  (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, marketplace_version_id, granted_permissions, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(plugin_slug), 'enabled', sqlc.arg(config_ciphertext), sqlc.arg(config_nonce),
        sqlc.arg(marketplace_version_id), sqlc.arg(granted_permissions), sqlc.arg(at), sqlc.arg(at))
RETURNING *;

-- name: UpdateInstall :one
UPDATE plugin_installs
SET status = 'enabled', config_ciphertext = sqlc.arg(config_ciphertext), config_nonce = sqlc.arg(config_nonce),
    marketplace_version_id = sqlc.arg(marketplace_version_id), granted_permissions = sqlc.arg(granted_permissions), updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DisableVersionInstalls :exec
UPDATE plugin_installs SET status = 'disabled', updated_at = sqlc.arg(at) WHERE marketplace_version_id = sqlc.arg(version_id);

-- name: InsertExecution :exec
INSERT INTO marketplace_executions (id, tenant_id, install_id, version_id, tool_name, status, created_at)
VALUES ($1, $2, $3, $4, $5, 'running', $6);

-- name: FinishExecution :exec
UPDATE marketplace_executions SET status = sqlc.arg(status), error_code = sqlc.narg(error_code), duration_ms = sqlc.narg(duration_ms)
WHERE id = sqlc.arg(id);

-- Capability broker (MarketplaceCapabilityBroker.swift).

-- name: ReadMemory :one
SELECT content, tags FROM memories WHERE tenant_id = $1 AND id = $2;

-- name: InsertMemory :exec
INSERT INTO memories (id, tenant_id, content, tags, origin_kind, origin_source_id, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(content), sqlc.narg(tags), 'import', sqlc.arg(origin_source_id), sqlc.arg(at), sqlc.arg(at));

-- name: VaultFileByPath :one
SELECT id, content_type, sha256 FROM vault_files WHERE tenant_id = $1 AND path = $2;

-- name: InsertVaultFile :exec
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(path), sqlc.arg(content_type), sqlc.arg(size_bytes), sqlc.arg(sha256), sqlc.arg(at), sqlc.arg(at));

-- name: UpdateVaultFile :exec
UPDATE vault_files SET content_type = sqlc.arg(content_type), size_bytes = sqlc.arg(size_bytes), sha256 = sqlc.arg(sha256), updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id);
