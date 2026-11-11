-- Settings fixtures (parity/scenarios/settings, parity/scenarios-switched/agents/settings).
-- One user per file so settingsByUser (30 a minute) never fills.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at) VALUES
  ('5e770000-0000-4000-8000-0000000000a1', 'st-amy@parity.test', 'x', true, 'st_amy', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('5e770000-0000-4000-8000-0000000000b2', 'st-bea@parity.test', 'x', true, 'st_bea', 'trial', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- The row without its random id, clock columns or ciphertext bytes.
CREATE VIEW parity_st_hermes AS
  SELECT tenant_id, base_url, name,
         auth_header_ciphertext IS NOT NULL AS has_auth,
         verified_at IS NOT NULL AS verified,
         -- Swift encodes a Dictionary-backed struct in random key order: compare as jsonb.
         capabilities::jsonb AS capabilities, capabilities_checked_at IS NOT NULL AS checked
    FROM user_hermes_config
   WHERE tenant_id IN ('5e770000-0000-4000-8000-0000000000a1', '5e770000-0000-4000-8000-0000000000b2');
