-- Session fixtures (parity/scenarios/auth, parity/scenarios/app/02-me.yaml, and the cross
-- state's parity/scenarios-switched/cross). One user per case, so a write in one scenario
-- never feeds another. Codes are stored as sha256 hex, as AuthService writes them:
--   111111 reset, 222222 email verification, 333333 MFA.
-- Passwords: parity-login-pass-1 ($2a$, as Go writes it) and parity-login-pass-2 ($2b$, as
-- Swift writes it), both cost 12. Every far expiry is 2100-01-01.

INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, lockout_until, failed_login_attempts, created_at, updated_at) VALUES
  ('a0700000-0000-4000-8000-0000000000a1', 'auth-login@parity.test',     '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', false, 'auth-login',     'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a2', 'auth-2b@parity.test',        '$2b$12$T6Hil/HVSJrG9e7Qy5ksreANU5JHrxfJ5qESWsRwKW2PcICpW8Rtq', true,  'auth-2b',        'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a3', 'auth-locked@parity.test',    '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-locked',    'trial', 'none', '2100-01-01T00:00:00Z', 5, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a4', 'auth-lockme@parity.test',    '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-lockme',    'trial', 'none', '2026-01-01T00:00:00Z', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a5', 'auth-reset@parity.test',     '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-reset',     'trial', 'none', NULL, 3, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a6', 'auth-resetlock@parity.test', '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-resetlock', 'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a7', 'auth-resetexp@parity.test',  '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-resetexp',  'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a8', 'auth-verify@parity.test',    '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', false, 'auth-verify',    'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000a9', 'auth-verifylock@parity.test','$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', false, 'auth-verifylock','trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000aa', 'auth-mfa@parity.test',       '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-mfa',       'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000ab', 'auth/me@parity.test',        '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-me',        'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000ac', 'auth-founder@parity.test',   '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-founder',   'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000ad', 'auth-refresh@parity.test',   '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-refresh',   'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000ae', 'auth-ultimate@parity.test',  '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-ultimate',  'trial', 'ultimate', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  -- Cross-server state only (parity/scenarios-switched/cross): one user per direction.
  ('a0700000-0000-4000-8000-0000000000b1', 'auth-x-reset@parity.test',   '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-x-reset',   'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000b2', 'auth-x-mfa@parity.test',     '$2b$12$T6Hil/HVSJrG9e7Qy5ksreANU5JHrxfJ5qESWsRwKW2PcICpW8Rtq', true,  'auth-x-mfa',     'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- password_reset_tokens: a5 valid, a6 locked, a7 expired, b1 valid (cross).
INSERT INTO password_reset_tokens (id, tenant_id, code_hash, expires_at, used_at, failed_attempts, locked_until, created_at) VALUES
  ('a0710000-0000-4000-8000-0000000000a5', 'a0700000-0000-4000-8000-0000000000a5', 'bcb15f821479b4d5772bd0ca866c00ad5f926e3580720659cc80d39c9d09802a', '2100-01-01T00:00:00Z', NULL, 0, NULL, '2026-01-01T00:00:00Z'),
  ('a0710000-0000-4000-8000-0000000000a6', 'a0700000-0000-4000-8000-0000000000a6', 'bcb15f821479b4d5772bd0ca866c00ad5f926e3580720659cc80d39c9d09802a', '2100-01-01T00:00:00Z', NULL, 5, '2100-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0710000-0000-4000-8000-0000000000a7', 'a0700000-0000-4000-8000-0000000000a7', 'bcb15f821479b4d5772bd0ca866c00ad5f926e3580720659cc80d39c9d09802a', '2020-01-01T00:00:00Z', NULL, 0, NULL, '2026-01-01T00:00:00Z'),
  ('a0710000-0000-4000-8000-0000000000b1', 'a0700000-0000-4000-8000-0000000000b1', 'bcb15f821479b4d5772bd0ca866c00ad5f926e3580720659cc80d39c9d09802a', '2100-01-01T00:00:00Z', NULL, 0, NULL, '2026-01-01T00:00:00Z');

-- email_verification_tokens: a8 valid, a9 valid (driven to its lock).
INSERT INTO email_verification_tokens (id, tenant_id, code_hash, expires_at, used_at, failed_attempts, locked_until, created_at) VALUES
  ('a0720000-0000-4000-8000-0000000000a8', 'a0700000-0000-4000-8000-0000000000a8', '4cc8f4d609b717356701c57a03e737e5ac8fe885da8c7163d3de47e01849c635', '2100-01-01T00:00:00Z', NULL, 0, NULL, '2026-01-01T00:00:00Z'),
  ('a0720000-0000-4000-8000-0000000000a9', 'a0700000-0000-4000-8000-0000000000a9', '4cc8f4d609b717356701c57a03e737e5ac8fe885da8c7163d3de47e01849c635', '2100-01-01T00:00:00Z', NULL, 0, NULL, '2026-01-01T00:00:00Z');

-- mfa_challenges for auth-mfa: c1 valid, c2 consumed, c3 expired, c4 one failure from burnt;
-- b2 valid for the cross state.
INSERT INTO mfa_challenges (id, tenant_id, purpose, channel, destination, code_hash, expires_at, consumed_at, failed_attempts, resend_count, last_sent_at, created_at) VALUES
  ('a07300c1-0000-4000-8000-000000000001', 'a0700000-0000-4000-8000-0000000000aa', 'login', 'email', 'auth-mfa@parity.test', '68487dc295052aa79c530e283ce698b8c6bb1b42ff0944252e1910dbecdc5425', '2100-01-01T00:00:00Z', NULL, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a07300c2-0000-4000-8000-000000000002', 'a0700000-0000-4000-8000-0000000000aa', 'login', 'email', 'auth-mfa@parity.test', '68487dc295052aa79c530e283ce698b8c6bb1b42ff0944252e1910dbecdc5425', '2100-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a07300c3-0000-4000-8000-000000000003', 'a0700000-0000-4000-8000-0000000000aa', 'login', 'email', 'auth-mfa@parity.test', '68487dc295052aa79c530e283ce698b8c6bb1b42ff0944252e1910dbecdc5425', '2020-01-01T00:00:00Z', NULL, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a07300c4-0000-4000-8000-000000000004', 'a0700000-0000-4000-8000-0000000000aa', 'login', 'email', 'auth-mfa@parity.test', '68487dc295052aa79c530e283ce698b8c6bb1b42ff0944252e1910dbecdc5425', '2100-01-01T00:00:00Z', NULL, 4, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a07300b2-0000-4000-8000-0000000000b2', 'a0700000-0000-4000-8000-0000000000b2', 'login', 'email', 'auth-x-mfa@parity.test', '68487dc295052aa79c530e283ce698b8c6bb1b42ff0944252e1910dbecdc5425', '2100-01-01T00:00:00Z', NULL, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- refresh_tokens for auth-refresh: one live, one expired, one revoked. Raw values in
-- parity/scenarios/auth/04-refresh.yaml.
INSERT INTO refresh_tokens (id, tenant_id, token_hash, expires_at, revoked_at, created_at) VALUES
  ('a0740000-0000-4000-8000-000000000001', 'a0700000-0000-4000-8000-0000000000ad', '9859a36fb2ff27c271f8c73a9f3b9163bb1763c27d7d06bafe800f0824ff1896', '2100-01-01T00:00:00Z', NULL, '2026-01-01T00:00:00Z'),
  ('a0740000-0000-4000-8000-000000000002', 'a0700000-0000-4000-8000-0000000000ad', 'd7d6403ead6be1efe9bf5cea7d7cff04bd43cf9b0b9cc3bf3f6c2bc9780c60f4', '2020-01-01T00:00:00Z', NULL, '2020-01-01T00:00:00Z'),
  ('a0740000-0000-4000-8000-000000000003', 'a0700000-0000-4000-8000-0000000000ad', '4f6888055da4a009d99a9500cc247df0d5fbf6153728bcf28ecd24f41cdae569', '2100-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

-- Wave 8B sign-in flows (parity/scenarios/auth/08-flows.yaml, 09-pairing.yaml): a phone
-- account and a magic-link account with their identities, so a verify signs in to a known
-- user and writes nothing but a refresh token; and the account that approves pairings.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, lockout_until, failed_login_attempts, created_at, updated_at) VALUES
  ('a0700000-0000-4000-8000-0000000000c1', '15550001001@phone.luminavault.local', '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', false, 'oauth-c1c1c1c1', 'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000c2', 'auth-magic@parity.test',              '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-magic',     'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0700000-0000-4000-8000-0000000000c3', 'auth-pair@parity.test',               '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true,  'auth-pair',      'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO oauth_identities (id, tenant_id, provider, provider_user_id, email, email_verified, created_at) VALUES
  ('a0750000-0000-4000-8000-0000000000c1', 'a0700000-0000-4000-8000-0000000000c1', 'phone',            '+15550001001',           '15550001001@phone.luminavault.local', false, '2026-01-01T00:00:00Z'),
  ('a0750000-0000-4000-8000-0000000000c2', 'a0700000-0000-4000-8000-0000000000c2', 'email_magic_link', 'auth-magic@parity.test', 'auth-magic@parity.test',              true,  '2026-01-01T00:00:00Z');
