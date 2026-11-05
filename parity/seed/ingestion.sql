-- Ingestion fixtures (parity/scenarios/ingestion). Loaded after base.sql.
--
-- Every item that reaches `complete` is seeded, because its vault file is named
-- after the item id: a generated id would put a random path in vault_files, and
-- later modules snapshot that table. Scenarios that create items never complete
-- them. The worker is off in parity (LV_ENVIRONMENT=test), so seeded rows hold
-- the states only the worker can write. Ids use the 1a6e prefix; no time here is
-- relative to now().

INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
VALUES
  ('1a6e0000-0000-4000-8000-0000000000a1', 'ing-ann@parity.test', 'x', true, 'ing_ann', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a2', 'ing-src@parity.test', 'x', true, 'ing_src', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a3', 'ing-zed@parity.test', 'x', true, 'ing_zed', 'lapsed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a4', 'ing-fay@parity.test', 'x', true, 'ing_fay', 'free',   '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a5', 'ing-pam@parity.test', 'x', true, 'ing_pam', 'pro',    '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a6', 'ing-lis@parity.test', 'x', true, 'ing_lis', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a7', 'ing-new@parity.test', 'x', true, 'ing_new', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a8', 'ing-oli@parity.test', 'x', true, 'ing_oli', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000a9', 'ing-vic@parity.test', 'x', true, 'ing_vic', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('1a6e0000-0000-4000-8000-0000000000aa', 'ing-gra@parity.test', 'x', true, 'ing_gra', 'trial',  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', created_at, updated_at FROM users WHERE id::text LIKE '1a6e0000-%';
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
SELECT ('1a6e7000' || substr(id::text, 9))::uuid, id, id, 'admin', true, id, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
FROM users WHERE id::text LIKE '1a6e0000-%';

-- A team vault: oli owns it, vic only reads. Its tenant id is no user, so the
-- quota check falls back to the trial tier.
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('1a6e2000-0000-4000-8000-000000000001', 'Ingestion Team', '1a6e0000-0000-4000-8000-0000000000a8', '1a6e0000-0000-4000-8000-0000000000a8', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');
INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES
  ('1a6e2200-0000-4000-8000-000000000001', '1a6e2000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a8', 'owner',  '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a6e2200-0000-4000-8000-000000000002', '1a6e2000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a9', 'member', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');
INSERT INTO vaults (id, team_id, name, created_at, updated_at)
VALUES ('1a6e2100-0000-4000-8000-000000000001', '1a6e2000-0000-4000-8000-000000000001', 'Shared Capture', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');
INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
VALUES
  ('1a6e2300-0000-4000-8000-000000000001', '1a6e2100-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a8', 'admin',  true,  '1a6e0000-0000-4000-8000-0000000000a8', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a6e2300-0000-4000-8000-000000000002', '1a6e2100-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a9', 'viewer', false, '1a6e0000-0000-4000-8000-0000000000a8', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- ann's space picks the upload folder; src's space is foreign to ann.
INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
VALUES
  ('1a6e1000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a1', 'Research', 'research', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('1a6e1000-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000a2', 'Elsewhere', 'elsewhere', 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

-- Rows behind the quota sums (no file behind them) and the source route's two
-- rows whose lookups fail: the file is missing, the row is another tenant's.
INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
VALUES
  ('1a6e6000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a4', 'big.pdf', 'application/pdf', 1073741000, 'aa', '2026-01-03T00:00:01Z', '2026-01-03T00:00:01Z'),
  ('1a6e6000-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000a5', 'huge.pdf', 'application/pdf', 106300440576, 'bb', '2026-01-03T00:00:02Z', '2026-01-03T00:00:02Z'),
  ('1a6e6000-0000-4000-8000-000000000003', '1a6e0000-0000-4000-8000-0000000000a2', 'missing/nofile.txt', 'text/plain', 9, 'cc', '2026-01-03T00:00:03Z', '2026-01-03T00:00:03Z'),
  ('1a6e6000-0000-4000-8000-000000000004', '1a6e0000-0000-4000-8000-0000000000a1', 'ann/foreign.txt', 'text/plain', 9, 'dd', '2026-01-03T00:00:04Z', '2026-01-03T00:00:04Z');

INSERT INTO ingestion_batches (id, tenant_id, space_id, state, created_at, updated_at)
VALUES
  -- ann: upload, space folder, retry, cancel, streams
  ('1a6e3000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'active', '2026-02-01T00:00:01Z', '2026-02-01T00:00:01Z'),
  ('1a6e3000-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e1000-0000-4000-8000-000000000001', 'active', '2026-02-01T00:00:02Z', '2026-02-01T00:00:02Z'),
  ('1a6e3000-0000-4000-8000-000000000003', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'attention', '2026-02-01T00:00:03Z', '2026-02-01T00:00:03Z'),
  ('1a6e3000-0000-4000-8000-000000000004', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'active', '2026-02-01T00:00:04Z', '2026-02-01T00:00:04Z'),
  ('1a6e3000-0000-4000-8000-000000000005', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'completed', '2026-02-01T00:00:05Z', '2026-02-01T00:00:05Z'),
  ('1a6e3000-0000-4000-8000-000000000006', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'attention', '2026-02-01T00:00:06Z', '2026-02-01T00:00:06Z'),
  ('1a6e3000-0000-4000-8000-000000000007', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'active', '2026-02-01T00:00:07Z', '2026-02-01T00:00:07Z'),
  ('1a6e3000-0000-4000-8000-000000000008', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'active', '2026-02-01T00:00:08Z', '2026-02-01T00:00:08Z'),
  ('1a6e3000-0000-4000-8000-000000000009', '1a6e0000-0000-4000-8000-0000000000a1', NULL, 'active', '2026-02-01T00:00:09Z', '2026-02-01T00:00:09Z'),
  -- src: the public source route
  ('1a6e3000-0000-4000-8000-0000000000b1', '1a6e0000-0000-4000-8000-0000000000a2', NULL, 'active', '2026-02-01T00:00:10Z', '2026-02-01T00:00:10Z'),
  -- team vault
  ('1a6e3000-0000-4000-8000-0000000000c1', '1a6e2100-0000-4000-8000-000000000001', NULL, 'active', '2026-02-01T00:00:11Z', '2026-02-01T00:00:11Z'),
  -- gra: every optional field of the batch DTO
  ('1a6e3000-0000-4000-8000-0000000000d1', '1a6e0000-0000-4000-8000-0000000000aa', NULL, 'attention', '2026-02-01T00:00:12Z', '2026-02-01T00:00:13Z');

-- lis: 52 batches, so the list stops at the newest 50.
INSERT INTO ingestion_batches (id, tenant_id, state, created_at, updated_at)
SELECT ('1a6e5000-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid, '1a6e0000-0000-4000-8000-0000000000a6', 'active',
       '2026-03-01T00:00:00Z'::timestamptz + n * interval '1 hour', '2026-03-01T00:00:00Z'::timestamptz + n * interval '1 hour'
FROM generate_series(1, 52) AS n;
INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, url, created_at, updated_at)
SELECT ('1a6e5100-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid, '1a6e0000-0000-4000-8000-0000000000a6',
       ('1a6e5000-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid, 'url', 'queued', 'https://example.test/' || n,
       '2026-03-01T00:00:00Z'::timestamptz + n * interval '1 hour', '2026-03-01T00:00:00Z'::timestamptz + n * interval '1 hour'
FROM generate_series(1, 52) AS n;

INSERT INTO memories (id, tenant_id, content, created_at)
VALUES ('1a6e8000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000aa', 'derived', '2026-02-01T00:00:00Z');

INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes, uploaded_bytes, expected_sha256, url, created_at, updated_at)
VALUES
  -- Upload flow (b1)
  ('1a6e4000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'ordered.txt', 'text/plain', 12, 0, NULL, NULL, '2026-02-02T00:00:01Z', '2026-02-02T00:00:01Z'),
  ('1a6e4000-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'wrongsha.txt', 'text/plain', 5, 0, '0000000000000000000000000000000000000000000000000000000000000000', NULL, '2026-02-02T00:00:02Z', '2026-02-02T00:00:02Z'),
  ('1a6e4000-0000-4000-8000-000000000003', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'rightsha.txt', 'text/plain', 5, 0, '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824', NULL, '2026-02-02T00:00:03Z', '2026-02-02T00:00:03Z'),
  ('1a6e4000-0000-4000-8000-000000000004', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'overflow.txt', 'text/plain', 4, 0, NULL, NULL, '2026-02-02T00:00:04Z', '2026-02-02T00:00:04Z'),
  ('1a6e4000-0000-4000-8000-000000000005', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'tool.exe', 'application/octet-stream', 3, 0, NULL, NULL, '2026-02-02T00:00:05Z', '2026-02-02T00:00:05Z'),
  ('1a6e4000-0000-4000-8000-000000000006', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'mismatch.txt', 'image/png', 3, 0, NULL, NULL, '2026-02-02T00:00:06Z', '2026-02-02T00:00:06Z'),
  ('1a6e4000-0000-4000-8000-000000000007', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', '../../etc/résumé final (1)!!.MD', 'text/markdown; charset=utf-8', 3, 0, NULL, NULL, '2026-02-02T00:00:07Z', '2026-02-02T00:00:07Z'),
  ('1a6e4000-0000-4000-8000-000000000008', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'whole.txt', 'text/plain', 8388608, 0, NULL, NULL, '2026-02-02T00:00:08Z', '2026-02-02T00:00:08Z'),
  ('1a6e4000-0000-4000-8000-000000000009', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'url', 'queued', NULL, NULL, NULL, 0, NULL, 'https://example.test/queued', '2026-02-02T00:00:09Z', '2026-02-02T00:00:09Z'),
  ('1a6e4000-0000-4000-8000-00000000000a', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'partial.txt', 'text/plain', 10, 0, NULL, NULL, '2026-02-02T00:00:10Z', '2026-02-02T00:00:10Z'),
  ('1a6e4000-0000-4000-8000-00000000000b', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', 'chunked.txt', 'text/plain', 7, 0, NULL, NULL, '2026-02-02T00:00:11Z', '2026-02-02T00:00:11Z'),
  ('1a6e4000-0000-4000-8000-00000000000c', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'cancelled', 'gone.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:12Z', '2026-02-02T00:00:12Z'),
  ('1a6e4000-0000-4000-8000-00000000000d', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', '!!!.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:13Z', '2026-02-02T00:00:13Z'),
  ('1a6e4000-0000-4000-8000-00000000000e', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000001', 'file', 'awaiting_upload', '---', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:14Z', '2026-02-02T00:00:14Z'),
  -- Space folder (b2)
  ('1a6e4000-0000-4000-8000-000000000010', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000002', 'file', 'awaiting_upload', 'filed.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:15Z', '2026-02-02T00:00:15Z'),
  -- Team vault
  ('1a6e4000-0000-4000-8000-0000000000c1', '1a6e2100-0000-4000-8000-000000000001', '1a6e3000-0000-4000-8000-0000000000c1', 'file', 'awaiting_upload', 'team.txt', 'text/plain', 9, 0, NULL, NULL, '2026-02-02T00:00:16Z', '2026-02-02T00:00:16Z'),
  -- Streams: a chunk for b7, and b8's only item, whose cancel ends the stream
  ('1a6e4000-0000-4000-8000-000000000040', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000007', 'file', 'awaiting_upload', 'streamed.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:17Z', '2026-02-02T00:00:17Z'),
  ('1a6e4000-0000-4000-8000-000000000041', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000008', 'file', 'awaiting_upload', 'stopped.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:18Z', '2026-02-02T00:00:18Z'),
  -- Cancel flow (b4): chunks to discard; completed is a no-op
  ('1a6e4000-0000-4000-8000-000000000030', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000004', 'file', 'awaiting_upload', 'discard.txt', 'text/plain', 3, 0, NULL, NULL, '2026-02-02T00:00:19Z', '2026-02-02T00:00:19Z');

-- Source route: the live token 'a' x 64 and 'e' x 64 items are uploaded by the
-- scenario, and complete doesn't touch the token columns.
INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes, vault_file_id, source_token_hash, source_token_expires_at, created_at, updated_at)
VALUES
  ('1a6e4000-0000-4000-8000-0000000000b1', '1a6e0000-0000-4000-8000-0000000000a2', '1a6e3000-0000-4000-8000-0000000000b1', 'file', 'awaiting_upload', 'so"urce "name.txt', 'text/plain; charset=utf-8', 1024, NULL,
   encode(sha256(convert_to(repeat('a', 64), 'UTF8')), 'hex'), '2099-01-01T00:00:00Z', '2026-02-03T00:00:01Z', '2026-02-03T00:00:01Z'),
  ('1a6e4000-0000-4000-8000-0000000000b2', '1a6e0000-0000-4000-8000-0000000000a2', '1a6e3000-0000-4000-8000-0000000000b1', 'file', 'queued', 'expired.txt', 'text/plain', 9, '1a6e6000-0000-4000-8000-000000000003',
   encode(sha256(convert_to(repeat('b', 64), 'UTF8')), 'hex'), '2020-01-01T00:00:00Z', '2026-02-03T00:00:02Z', '2026-02-03T00:00:02Z'),
  ('1a6e4000-0000-4000-8000-0000000000b3', '1a6e0000-0000-4000-8000-0000000000a2', '1a6e3000-0000-4000-8000-0000000000b1', 'file', 'queued', 'nofile.txt', 'text/plain', 9, '1a6e6000-0000-4000-8000-000000000003',
   encode(sha256(convert_to(repeat('c', 64), 'UTF8')), 'hex'), '2099-01-01T00:00:00Z', '2026-02-03T00:00:03Z', '2026-02-03T00:00:03Z'),
  ('1a6e4000-0000-4000-8000-0000000000b4', '1a6e0000-0000-4000-8000-0000000000a2', '1a6e3000-0000-4000-8000-0000000000b1', 'file', 'queued', 'foreign.txt', 'text/plain', 9, '1a6e6000-0000-4000-8000-000000000004',
   encode(sha256(convert_to(repeat('d', 64), 'UTF8')), 'hex'), '2099-01-01T00:00:00Z', '2026-02-03T00:00:04Z', '2026-02-03T00:00:04Z'),
  ('1a6e4000-0000-4000-8000-0000000000b5', '1a6e0000-0000-4000-8000-0000000000a2', '1a6e3000-0000-4000-8000-0000000000b1', 'file', 'awaiting_upload', 'small.txt', 'text/plain', 5, NULL,
   encode(sha256(convert_to(repeat('e', 64), 'UTF8')), 'hex'), '2099-01-01T00:00:00Z', '2026-02-03T00:00:05Z', '2026-02-03T00:00:05Z');

-- Retry (b3) and cancel (b4) states, with all the transient columns set.
INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes, error_message, attempts, next_attempt_at, lease_expires_at, source_token_hash, source_token_expires_at, terminal_notified_at, created_at, updated_at)
VALUES
  ('1a6e4000-0000-4000-8000-000000000020', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000003', 'file', 'failed', 'failed.txt', 'text/plain', 3, 'upstream unavailable', 2, '2099-01-01T00:00:00Z', '2099-01-01T00:00:00Z', 'f0', '2099-01-01T00:00:00Z', '2026-01-05T00:00:00Z', '2026-02-04T00:00:01Z', '2026-02-04T00:00:01Z'),
  ('1a6e4000-0000-4000-8000-000000000021', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000003', 'file', 'blocked_capability', 'blocked.txt', 'text/plain', 3, 'no capability', 3, NULL, NULL, NULL, NULL, '2026-01-05T00:00:00Z', '2026-02-04T00:00:02Z', '2026-02-04T00:00:02Z'),
  ('1a6e4000-0000-4000-8000-000000000022', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000003', 'file', 'extracting', 'busy.txt', 'text/plain', 3, NULL, 1, NULL, '2099-01-01T00:00:00Z', 'f2', '2099-01-01T00:00:00Z', NULL, '2026-02-04T00:00:03Z', '2026-02-04T00:00:03Z'),
  ('1a6e4000-0000-4000-8000-000000000031', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000004', 'file', 'completed', 'done.txt', 'text/plain', 3, NULL, 1, NULL, NULL, NULL, NULL, NULL, '2026-02-04T00:00:04Z', '2026-02-04T00:00:04Z'),
  ('1a6e4000-0000-4000-8000-000000000032', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000004', 'file', 'failed', 'broken.txt', 'text/plain', 3, 'broken', 3, '2099-01-01T00:00:00Z', '2099-01-01T00:00:00Z', 'f3', '2099-01-01T00:00:00Z', '2026-01-05T00:00:00Z', '2026-02-04T00:00:05Z', '2026-02-04T00:00:05Z'),
  ('1a6e4000-0000-4000-8000-000000000033', '1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000004', 'url', 'saving', NULL, NULL, NULL, NULL, 1, NULL, '2099-01-01T00:00:00Z', NULL, NULL, NULL, '2026-02-04T00:00:06Z', '2026-02-04T00:00:06Z');

-- gra: a finished item with a derived memory and graph, a state the enum doesn't
-- know (it reads as failed), and an item reusing the first one's analysis.
INSERT INTO ingestion_items (id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes, uploaded_bytes, url, memory_id, summary, error_message, credibility, attempts, content_sha256, reused_from_item_id, graph_ready_at, created_at, updated_at)
VALUES
  ('1a6e4000-0000-4000-8000-0000000000d1', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e3000-0000-4000-8000-0000000000d1', 'url', 'completed', NULL, 'text/html', NULL, 0, 'https://example.test/a/b?c=d', '1a6e8000-0000-4000-8000-000000000001', 'a summary', NULL,
   '{"score": 72, "confidence": 0.75, "signals": ["peer reviewed", "cited/widely"], "rationale": "sound", "version": "v1"}', 1, 'abc123', NULL, '2026-02-05T00:00:00Z', '2026-02-05T00:00:01Z', '2026-02-05T00:00:02Z'),
  ('1a6e4000-0000-4000-8000-0000000000d2', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e3000-0000-4000-8000-0000000000d1', 'file', 'weird', 'odd.txt', 'text/plain', 3, 3, NULL, NULL, NULL, 'an error', NULL, 3, NULL, NULL, NULL, '2026-02-05T00:00:03Z', '2026-02-05T00:00:04Z'),
  ('1a6e4000-0000-4000-8000-0000000000d3', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e3000-0000-4000-8000-0000000000d1', 'file', 'analyzing', 'reused.txt', 'text/plain', 3, 3, NULL, NULL, 'b summary', NULL,
   '{"confidence": 1, "signals": [], "rationale": "personal", "version": "v2"}', 1, 'abc123', '1a6e4000-0000-4000-8000-0000000000d1', NULL, '2026-02-05T00:00:05Z', '2026-02-05T00:00:06Z');

-- Node 3 is a claim, so it isn't listed and the edge to it is filtered out.
-- Node 4 has its evidence on another memory.
INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence, created_at, updated_at)
VALUES
  ('1a6e9000-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000aa', 'entity', 'zeta', 'Zeta', 0.5, '2026-02-05T00:00:00Z', '2026-02-05T00:00:00Z'),
  ('1a6e9000-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000aa', 'entity', 'alpha', 'Alpha', 0.25, '2026-02-05T00:00:00Z', '2026-02-05T00:00:00Z'),
  ('1a6e9000-0000-4000-8000-000000000003', '1a6e0000-0000-4000-8000-0000000000aa', 'claim', 'claim', 'A claim', 1, '2026-02-05T00:00:00Z', '2026-02-05T00:00:00Z');
INSERT INTO knowledge_edges (id, tenant_id, from_node_id, to_node_id, predicate, state, confidence, evidence_fingerprint, created_at, updated_at)
VALUES
  ('1a6e9100-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e9000-0000-4000-8000-000000000001', '1a6e9000-0000-4000-8000-000000000002', 'mentions', 'asserted', 0.5, 'f1', '2026-02-05T00:00:00Z', '2026-02-05T00:00:00Z'),
  ('1a6e9100-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e9000-0000-4000-8000-000000000001', '1a6e9000-0000-4000-8000-000000000003', 'about', 'asserted', 0.5, 'f2', '2026-02-05T00:00:00Z', '2026-02-05T00:00:00Z');
INSERT INTO knowledge_evidence (id, tenant_id, node_id, edge_id, memory_id, quote, created_at)
VALUES
  ('1a6e9200-0000-4000-8000-000000000001', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e9000-0000-4000-8000-000000000001', NULL, '1a6e8000-0000-4000-8000-000000000001', 'z', '2026-02-05T00:00:00Z'),
  ('1a6e9200-0000-4000-8000-000000000002', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e9000-0000-4000-8000-000000000002', NULL, '1a6e8000-0000-4000-8000-000000000001', 'a', '2026-02-05T00:00:00Z'),
  ('1a6e9200-0000-4000-8000-000000000003', '1a6e0000-0000-4000-8000-0000000000aa', '1a6e9000-0000-4000-8000-000000000003', NULL, '1a6e8000-0000-4000-8000-000000000001', 'c', '2026-02-05T00:00:00Z'),
  ('1a6e9200-0000-4000-8000-000000000004', '1a6e0000-0000-4000-8000-0000000000aa', NULL, '1a6e9100-0000-4000-8000-000000000001', '1a6e8000-0000-4000-8000-000000000001', 'the first quote', '2026-02-05T00:00:01Z'),
  ('1a6e9200-0000-4000-8000-000000000005', '1a6e0000-0000-4000-8000-0000000000aa', NULL, '1a6e9100-0000-4000-8000-000000000001', '1a6e8000-0000-4000-8000-000000000001', 'a later quote', '2026-02-05T00:00:02Z'),
  ('1a6e9200-0000-4000-8000-000000000006', '1a6e0000-0000-4000-8000-0000000000aa', NULL, '1a6e9100-0000-4000-8000-000000000002', '1a6e8000-0000-4000-8000-000000000001', 'not listed', '2026-02-05T00:00:03Z');

-- b5's stream: ids 1 to 7, in this order. 3 has a type the enum doesn't know, so
-- it's dropped. 5's state is unknown, so the key is omitted. 7 is another
-- tenant's row on the same batch, and is not read. No other event is seeded, so
-- the scenarios know these ids.
INSERT INTO ingestion_events (tenant_id, batch_id, item_id, type, state, uploaded_bytes, created_at)
VALUES
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', '1a6e4000-0000-4000-8000-000000000040', 'state_changed', 'queued', 5, '2026-02-06T00:00:01Z'),
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', '1a6e4000-0000-4000-8000-000000000040', 'progress', 'awaiting_upload', 3, '2026-02-06T00:00:02Z'),
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', NULL, 'weird', 'queued', NULL, '2026-02-06T00:00:03Z'),
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', NULL, 'terminal', NULL, NULL, '2026-02-06T00:00:04Z'),
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', NULL, 'snapshot', 'bogus', 0, '2026-02-06T00:00:05Z'),
  ('1a6e0000-0000-4000-8000-0000000000a1', '1a6e3000-0000-4000-8000-000000000005', NULL, 'heartbeat', 'completed', NULL, '2026-02-06T00:00:06Z'),
  ('b0b00000-0000-4000-8000-000000000002', '1a6e3000-0000-4000-8000-000000000005', NULL, 'terminal', 'failed', NULL, '2026-02-06T00:00:07Z');
