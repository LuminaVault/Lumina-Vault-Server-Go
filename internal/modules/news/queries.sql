-- News ticker install state (Swift: PluginService.installState(tenantID:slug:)
-- in Sources/App/Plugins/PluginService.swift). (tenant_id, plugin_slug) is unique.

-- name: NewsTickerInstall :one
SELECT status, config_ciphertext, config_nonce
FROM plugin_installs
WHERE tenant_id = $1 AND plugin_slug = 'news-ticker'
LIMIT 1;
