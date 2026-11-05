-- /v1/transcribe, /v1/audio and /v1/tts fixtures (parity/scenarios/llm-voice, tts,
-- scenarios-switched). The limits are low, so many users. vo_rev and vo_old have an
-- audio token epoch, on either side of the iat of the static scoped tokens.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, hermes_audio_token_epoch, created_at, updated_at)
VALUES
  ('70ce0000-0000-4000-8000-0000000000a0', 'vo_type@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_type', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a1', 'vo_type2@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_type2', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a2', 'vo_mime1@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_mime1', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a3', 'vo_mime2@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_mime2', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a4', 'vo_chan@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_chan', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a5', 'vo_chan2@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_chan2', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a6', 'vo_cap@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_cap', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a7', 'vo_form1@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_form1', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a8', 'vo_form2@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_form2', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a9', 'vo_form3@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_form3', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000aa', 'vo_form4@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_form4', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ab', 'vo_rl@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_rl', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ac', 'vo_arl@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_arl', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ad', 'vo_srl@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_srl', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ae', 'vo_misc@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_misc', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000af', 'vo_tok@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_tok', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b0', 'vw_a@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vw_a', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b1', 'vw_b@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vw_b', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b2', 'vw_c@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vw_c', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b3', 'vt_a@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vt_a', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b4', 'vt_b@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vt_b', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b5', 'vt_c@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vt_c', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b6', 'vt_rl@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vt_rl', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b7', 'vt_big@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vt_big', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b8', 'vs_a@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vs_a', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c0', 'vo_rev@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_rev', 'trial', 'none', '2026-06-01T00:00:00Z', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c1', 'vo_old@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_old', 'trial', 'none', '2025-01-01T00:00:00Z', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c2', 'vo_free@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vo_free', 'free', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d0', 'vb_free@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_free', 'free', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d1', 'vb_lapsed@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_lapsed', 'lapsed', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d2', 'vb_arch@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_arch', 'archived', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d3', 'vb_trial@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_trial', 'trial', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d4', 'vb_pro@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_pro', 'pro', 'none', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d5', 'vb_over@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'vb_over', 'free', 'ultimate', NULL, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
VALUES
  ('70ce0000-0000-4000-8000-0000000000a0', '70ce0000-0000-4000-8000-0000000000a0', 'vo_type''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a1', '70ce0000-0000-4000-8000-0000000000a1', 'vo_type2''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a2', '70ce0000-0000-4000-8000-0000000000a2', 'vo_mime1''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a3', '70ce0000-0000-4000-8000-0000000000a3', 'vo_mime2''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a4', '70ce0000-0000-4000-8000-0000000000a4', 'vo_chan''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a5', '70ce0000-0000-4000-8000-0000000000a5', 'vo_chan2''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a6', '70ce0000-0000-4000-8000-0000000000a6', 'vo_cap''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a7', '70ce0000-0000-4000-8000-0000000000a7', 'vo_form1''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a8', '70ce0000-0000-4000-8000-0000000000a8', 'vo_form2''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000a9', '70ce0000-0000-4000-8000-0000000000a9', 'vo_form3''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000aa', '70ce0000-0000-4000-8000-0000000000aa', 'vo_form4''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ab', '70ce0000-0000-4000-8000-0000000000ab', 'vo_rl''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ac', '70ce0000-0000-4000-8000-0000000000ac', 'vo_arl''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ad', '70ce0000-0000-4000-8000-0000000000ad', 'vo_srl''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000ae', '70ce0000-0000-4000-8000-0000000000ae', 'vo_misc''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000af', '70ce0000-0000-4000-8000-0000000000af', 'vo_tok''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b0', '70ce0000-0000-4000-8000-0000000000b0', 'vw_a''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b1', '70ce0000-0000-4000-8000-0000000000b1', 'vw_b''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b2', '70ce0000-0000-4000-8000-0000000000b2', 'vw_c''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b3', '70ce0000-0000-4000-8000-0000000000b3', 'vt_a''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b4', '70ce0000-0000-4000-8000-0000000000b4', 'vt_b''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b5', '70ce0000-0000-4000-8000-0000000000b5', 'vt_c''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b6', '70ce0000-0000-4000-8000-0000000000b6', 'vt_rl''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b7', '70ce0000-0000-4000-8000-0000000000b7', 'vt_big''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000b8', '70ce0000-0000-4000-8000-0000000000b8', 'vs_a''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c0', '70ce0000-0000-4000-8000-0000000000c0', 'vo_rev''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c1', '70ce0000-0000-4000-8000-0000000000c1', 'vo_old''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000c2', '70ce0000-0000-4000-8000-0000000000c2', 'vo_free''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d0', '70ce0000-0000-4000-8000-0000000000d0', 'vb_free''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d1', '70ce0000-0000-4000-8000-0000000000d1', 'vb_lapsed''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d2', '70ce0000-0000-4000-8000-0000000000d2', 'vb_arch''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d3', '70ce0000-0000-4000-8000-0000000000d3', 'vb_trial''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d4', '70ce0000-0000-4000-8000-0000000000d4', 'vb_pro''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('70ce0000-0000-4000-8000-0000000000d5', '70ce0000-0000-4000-8000-0000000000d5', 'vb_over''s Vault', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z');

-- usage_events, without what can't compare: latency, the time, and a random key.
CREATE VIEW parity_voice_usage AS
SELECT tenant_id, metric, amount, source, idempotency_key IS NOT NULL AS keyed, metadata - 'latencyMs' AS metadata
FROM usage_events
WHERE metric = 'voice_transcribe';

-- usage_meter's TTS rows, without the day. /v1/tts writes them on its own goroutine, so
-- parity_tts_unmetered is what a step waits on.
CREATE VIEW parity_tts_usage AS
SELECT tenant_id, model, mtok_in, mtok_out, chars_out FROM usage_meter WHERE chars_out > 0;

CREATE VIEW parity_tts_unmetered AS
SELECT 1 AS waiting WHERE NOT EXISTS (SELECT 1 FROM usage_meter WHERE chars_out > 0);
