-- /v1/grok fixtures (parity/scenarios/grok, scenarios-switched/no-master-key).
-- api_server_key_* are sealed with the parity LV_SECRET_MASTER_KEY, for the tenant
-- each row belongs to, except grok_badkey's, which is sealed for grok_ann.
--
-- HermesContainerHandle.baseURL is http://<container_name>:8642, so the name in the
-- row is the host the server dials. grok-hermes-ann and grok-hermes-ben are network
-- aliases of the feeds stub (docker-compose.parity.yml), which answers on :8642.
-- grok-hermes-down resolves to nothing.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('670c0000-0000-4000-8000-0000000000a1', 'grok_ann@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_ann', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a2', 'grok_ben@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_ben', 'ultimate', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a3', 'grok_over@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_over', 'trial', 'pro', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a4', 'grok_nobox@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_nobox', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a5', 'grok_unlinked@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_unlinked', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a6', 'grok_badkey@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_badkey', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000a7', 'grok_down@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_down', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000b1', 'grok_trial@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_trial', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000b2', 'grok_free@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_free', 'free', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000b3', 'grok_lapsed@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_lapsed', 'lapsed', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000b4', 'grok_arch@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_arch', 'archived', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('670c0000-0000-4000-8000-0000000000b5', 'grok_archover@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'grok_archover', 'archived', 'ultimate', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

-- grok_unlinked has a container and only a Nous connection: linked to the wrong thing.
INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, nous_connected_at, created_at)
VALUES
  ('670ce000-0000-4000-8000-0000000000a1', '670c0000-0000-4000-8000-0000000000a1', 'grok-hermes-ann', 19101, '\xa94cb7e9ee8a0aea5abbee0e62193148c39e4b6362a57d1850d2593e2f6ca1c60d875d', '\x82532483188b577983a625ed', '2026-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z'),
  ('670ce000-0000-4000-8000-0000000000a2', '670c0000-0000-4000-8000-0000000000a2', 'grok-hermes-ben', 19102, '\xd0572780f582d214b92b14cbda9693aaa8c5e880dddd326c3aa7b983ec65a8a7bac0af', '\x4be0a13a4521dd7529bdbef6', '2026-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z'),
  ('670ce000-0000-4000-8000-0000000000a5', '670c0000-0000-4000-8000-0000000000a5', 'grok-hermes-unlinked', 19105, '\xf47c7e0f2302f7cb13fbf9195e6c6221b77f2bca4f635956a41b4179d32b3ba36f29861ab21cd8b5', '\x7df5ca43f01edd17205ddc1a', NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('670ce000-0000-4000-8000-0000000000a6', '670c0000-0000-4000-8000-0000000000a6', 'grok-hermes-badkey', 19106, '\x988f1710405206568ea83067b97c6357bf2dadefe93321caa8fe38d4981398e43031fa9f249b', '\x4319c0baeeff58942caa80b6', '2026-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z'),
  ('670ce000-0000-4000-8000-0000000000a7', '670c0000-0000-4000-8000-0000000000a7', 'grok-hermes-down', 19107, '\x15e0151b67b85033b6f177dd6a577a4c1478bfa0dd5419f9495e18504b6385867167466f', '\x0ce61305b00c6b225411fd8b', '2026-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z');
