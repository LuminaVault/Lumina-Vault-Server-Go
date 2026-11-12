-- Passkey fixtures (parity/scenarios/auth/08-webauthn.yaml, wave 8C). Rows are written as
-- Swift's Fluent model writes them (WebAuthnCredential, M11): credential_id is the padded
-- standard base64 of the id, public_key the COSE_Key bytes (here an ES256 key on the P-256
-- generator point, which no one holds the private half of), sign_count a bigint.
--   wa-ann: two credentials, listed and signed in against (the assertion never verifies).
--   wa-ben: three credentials, deleted by plain id, by percent-encoded id and by an id with "/".
--   wa-cy:  none; enrols.
INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, lockout_until, failed_login_attempts, created_at, updated_at) VALUES
  ('a08c0000-0000-4000-8000-0000000000a1', 'wa-ann@parity.test', '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true, 'wa-ann', 'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a08c0000-0000-4000-8000-0000000000b2', 'wa-ben@parity.test', '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true, 'wa-ben', 'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a08c0000-0000-4000-8000-0000000000c3', 'wa-cy@parity.test',  '$2a$12$Z3ZN5TnI9JPreL0bsr9age1.CVv1eYC2qRhVjCt81y9IaNTo3gNla', true, 'wa-cy',  'trial', 'none', NULL, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO webauthn_credentials (id, tenant_id, credential_id, public_key, sign_count, created_at, updated_at) VALUES
  ('a08c1000-0000-4000-8000-000000000001', 'a08c0000-0000-4000-8000-0000000000a1', '++++AQID',
   '\xa50102032620012158206b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2962258204fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5',
   0, '2026-02-01T10:00:00Z', '2026-02-01T10:00:00Z'),
  ('a08c1000-0000-4000-8000-000000000002', 'a08c0000-0000-4000-8000-0000000000a1', 'AAAAAAAAAAAAAAAAAAAAAAAAAAA=',
   '\xa50102032620012158206b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2962258204fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5',
   7, '2026-02-02T10:00:00Z', '2026-03-05T08:30:00Z'),
  ('a08c1000-0000-4000-8000-000000000003', 'a08c0000-0000-4000-8000-0000000000b2', 'Y3JlZGVudGlhbC1pZC0yNC1ieXRlcyEh',
   '\xa50102032620012158206b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2962258204fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5',
   0, '2026-02-03T10:00:00Z', NULL),
  ('a08c1000-0000-4000-8000-000000000004', 'a08c0000-0000-4000-8000-0000000000b2', '+/+/AQID',
   '\xa50102032620012158206b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2962258204fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5',
   0, '2026-02-04T10:00:00Z', '2026-02-04T10:00:00Z'),
  ('a08c1000-0000-4000-8000-000000000005', 'a08c0000-0000-4000-8000-0000000000b2', '++8+AQID',
   '\xa50102032620012158206b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2962258204fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5',
   0, '2026-02-05T10:00:00Z', '2026-02-05T10:00:00Z');
