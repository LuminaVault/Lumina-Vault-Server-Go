-- /v1/vision fixtures (parity/scenarios/vision, scenarios-switched). 10 embeds a
-- minute per user, so several users. Vectors are built against the stub adapter's
-- 1024 values of 0.1 padded with zeros to 1536.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
VALUES
  ('6b0e0000-0000-4000-8000-0000000000a1', 'vis_ct@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_ct', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a2', 'vis_lim@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_lim', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a3', 'vis_idx@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_idx', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a4', 'vis_rl@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_rl', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a5', 'vis_big@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_big', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a6', 'vis_srch@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_srch', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a7', 'vis_oth@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_oth', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a8', 'vis_coh@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_coh', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b1', 'vis_free@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_free', 'free', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b2', 'vis_lapsed@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_lapsed', 'lapsed', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b3', 'vis_arch@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_arch', 'archived', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b4', 'vis_trial@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_trial', 'trial', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b5', 'vis_pro@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_pro', 'pro', 'none', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b6', 'vis_over@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vis_over', 'free', 'ultimate', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('6b0e0000-0000-4000-8000-0000000000a1', '6b0e0000-0000-4000-8000-0000000000a1', 'vis_ct''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a2', '6b0e0000-0000-4000-8000-0000000000a2', 'vis_lim''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a3', '6b0e0000-0000-4000-8000-0000000000a3', 'vis_idx''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a4', '6b0e0000-0000-4000-8000-0000000000a4', 'vis_rl''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a5', '6b0e0000-0000-4000-8000-0000000000a5', 'vis_big''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a6', '6b0e0000-0000-4000-8000-0000000000a6', 'vis_srch''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a7', '6b0e0000-0000-4000-8000-0000000000a7', 'vis_oth''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000a8', '6b0e0000-0000-4000-8000-0000000000a8', 'vis_coh''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b1', '6b0e0000-0000-4000-8000-0000000000b1', 'vis_free''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b2', '6b0e0000-0000-4000-8000-0000000000b2', 'vis_lapsed''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b3', '6b0e0000-0000-4000-8000-0000000000b3', 'vis_arch''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b4', '6b0e0000-0000-4000-8000-0000000000b4', 'vis_trial''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b5', '6b0e0000-0000-4000-8000-0000000000b5', 'vis_pro''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000b6', '6b0e0000-0000-4000-8000-0000000000b6', 'vis_over''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO memories (id, tenant_id, content, embedding, review_state, origin_kind, created_at)
VALUES
  ('6b0e0000-0000-4000-8000-0000000000e1', '6b0e0000-0000-4000-8000-0000000000a6', 'Same as the stub vector', (array_fill(0.1::real, ARRAY[1024]) || array_fill(0::real, ARRAY[512]))::vector, 'auto', 'legacy', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e2', '6b0e0000-0000-4000-8000-0000000000a6', 'Near the stub vector', (array_fill(0.05::real, ARRAY[512]) || array_fill(0.1::real, ARRAY[512]) || array_fill(0::real, ARRAY[512]))::vector, 'auto', 'legacy', '2026-03-02T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e3', '6b0e0000-0000-4000-8000-0000000000a6', 'Far from the stub vector', (array_fill(0::real, ARRAY[1023]) || array_fill(0.1::real, ARRAY[513]))::vector, 'auto', 'legacy', NULL),
  ('6b0e0000-0000-4000-8000-0000000000e4', '6b0e0000-0000-4000-8000-0000000000a6', 'No embedding yet', NULL, 'auto', 'legacy', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e5', '6b0e0000-0000-4000-8000-0000000000a6', 'Rejected in review', (array_fill(0.1::real, ARRAY[1024]) || array_fill(0::real, ARRAY[512]))::vector, 'rejected', 'legacy', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e6', '6b0e0000-0000-4000-8000-0000000000a6', 'A model answer', (array_fill(0.05::real, ARRAY[512]) || array_fill(0.1::real, ARRAY[512]) || array_fill(0::real, ARRAY[512]))::vector, 'auto', 'chat', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e7', '6b0e0000-0000-4000-8000-0000000000a6', 'To index', NULL, 'auto', 'legacy', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e8', '6b0e0000-0000-4000-8000-0000000000a7', 'Another tenant', (array_fill(0.1::real, ARRAY[1024]) || array_fill(0::real, ARRAY[512]))::vector, 'auto', 'legacy', '2026-03-01T00:00:00Z'),
  ('6b0e0000-0000-4000-8000-0000000000e9', '6b0e0000-0000-4000-8000-0000000000a8', 'Cohere target', NULL, 'auto', 'legacy', '2026-03-01T00:00:00Z');

-- usage_meter without `day`: CURRENT_DATE would differ between the two servers' runs
-- if midnight UTC fell between them. A view, because snapshots read whole relations.
-- usage_meter has no unique key and both servers meter after answering with a
-- read-then-write, so back-to-back embeds can split one tenant-day into two rows
-- (a Swift weakness Go keeps). Compare the totals, not the row split.
CREATE VIEW parity_vision_usage AS
  SELECT tenant_id, model, sum(mtok_in) AS mtok_in, sum(mtok_out) AS mtok_out, sum(chars_out) AS chars_out
    FROM usage_meter WHERE model LIKE 'vision.%'
   GROUP BY tenant_id, model ORDER BY tenant_id, model;
