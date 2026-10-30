-- News ticker fixture (parity/scenarios/news): alice has the news-ticker
-- plugin installed and enabled, bob installed and disabled. The config is
-- empty (an unopenable SecretBox value reads as no config in Swift).
INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, created_at, updated_at)
VALUES
  ('9e000000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'news-ticker', 'enabled',  '\x', '\x', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000002', 'b0b00000-0000-4000-8000-000000000002', 'news-ticker', 'disabled', '\x', '\x', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');
