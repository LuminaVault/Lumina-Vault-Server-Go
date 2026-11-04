-- Marketplace parity seed (parity/scenarios/marketplace). Ids use the aa77 (users),
-- aa78 (publishers), aa79 (listings), aa7a (versions) and aa7b (other rows) prefixes.
-- mkt_admin and mkt_adm2 are admins.
--
-- Swift cannot insert a marketplace_versions row (config_fields is bound as jsonb[]
-- into a jsonb column), so POST .../versions is always a 409, and seeding the
-- first-party catalog fails after one listing, which breaks every list that reaches
-- it. This file therefore seeds the versions the scenarios need, and a "luminavault"
-- publisher so the catalog seed is skipped and the lists work. The seed's failure has
-- its own state: parity/seed-switched/catalog-unseeded.sql.

INSERT INTO users (id, email, password_hash, is_verified, username, is_admin, created_at, updated_at)
VALUES
  ('aa770000-0000-4000-8000-0000000000a1', 'mkt_admin@parity.test', 'x', true, 'mkt_admin', true,  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a2', 'mkt_pub@parity.test',   'x', true, 'mkt_pub',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a3', 'mkt_pub2@parity.test',  'x', true, 'mkt_pub2',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a4', 'mkt_pend@parity.test',  'x', true, 'mkt_pend',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a5', 'mkt_app@parity.test',   'x', true, 'mkt_app',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a6', 'mkt_app2@parity.test',  'x', true, 'mkt_app2',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a7', 'mkt_inst@parity.test',  'x', true, 'mkt_inst',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a8', 'mkt_rate@parity.test',  'x', true, 'mkt_rate',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000a9', 'mkt_run@parity.test',   'x', true, 'mkt_run',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000aa', 'mkt_cap@parity.test',   'x', true, 'mkt_cap',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000ab', 'mkt_val@parity.test',   'x', true, 'mkt_val',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000ac', 'mkt_val2@parity.test',  'x', true, 'mkt_val2',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000ad', 'mkt_read@parity.test',  'x', true, 'mkt_read',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000ae', 'mkt_none@parity.test',  'x', true, 'mkt_none',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000af', 'mkt_rej@parity.test',   'x', true, 'mkt_rej',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000b0', 'mkt_adm2@parity.test',  'x', true, 'mkt_adm2',  true,  '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000b1', 'mkt_wasm@parity.test',  'x', true, 'mkt_wasm',  false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000b2', 'mkt_up@parity.test',    'x', true, 'mkt_up',    false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('aa770000-0000-4000-8000-0000000000b3', 'mkt_bad@parity.test',   'x', true, 'mkt_bad',   false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', created_at, updated_at FROM users WHERE id::text LIKE 'aa770000-%';

INSERT INTO marketplace_publishers (id, owner_user_id, handle, display_name, bio, website_url, status, verified, created_at, updated_at)
VALUES
  ('aa780000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000a2', 'parity-pub', 'Parity Publisher', 'Seeded bio', 'https://pub.parity.test', 'approved', true, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('aa780000-0000-4000-8000-000000000002', 'aa770000-0000-4000-8000-0000000000a3', 'other-pub', 'Other Publisher', NULL, NULL, 'approved', true, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('aa780000-0000-4000-8000-000000000003', 'aa770000-0000-4000-8000-0000000000a4', 'waiting-pub', 'Waiting Publisher', NULL, NULL, 'pending', false, '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'),
  ('aa780000-0000-4000-8000-000000000004', 'aa770000-0000-4000-8000-0000000000af', 'turned-down', 'Turned Down', NULL, NULL, 'rejected', false, '2026-01-04T00:00:00Z', '2026-01-04T00:00:00Z'),
  ('aa780000-0000-4000-8000-000000000005', 'aa770000-0000-4000-8000-0000000000b1', 'wasm-pub', 'Wasm Publisher', NULL, NULL, 'approved', true, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('aa780000-0000-4000-8000-000000000006', 'aa770000-0000-4000-8000-0000000000b0', 'luminavault', 'LuminaVault', NULL, NULL, 'approved', true, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO marketplace_listings (id, publisher_id, slug, name, summary, description, category, icon_url, screenshots, status, featured, created_at, updated_at)
VALUES
  ('aa790000-0000-4000-8000-000000000001', 'aa780000-0000-4000-8000-000000000001', 'alpha-notes', 'Alpha Notes', 'Capture alpha notes', 'Long alpha description', 'capture', 'https://cdn.parity.test/alpha.png', '{https://cdn.parity.test/a1.png}', 'published', true, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('aa790000-0000-4000-8000-000000000002', 'aa780000-0000-4000-8000-000000000002', 'beta-tools', 'Beta Tools', 'Export things', 'Long beta description', 'export', NULL, '{}', 'published', false, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('aa790000-0000-4000-8000-000000000003', 'aa780000-0000-4000-8000-000000000001', 'draft-thing', 'Draft Thing', 'Not out yet', 'Long draft description', 'theme', NULL, '{}', 'draft', false, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('aa790000-0000-4000-8000-000000000004', 'aa780000-0000-4000-8000-000000000002', 'revoke-me', 'Revoke Me', 'Two approved versions', 'Long revoke description', 'skill', NULL, '{}', 'published', false, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('aa790000-0000-4000-8000-000000000005', 'aa780000-0000-4000-8000-000000000002', 'lost-wasm', 'Lost Wasm', 'Artifact is gone', 'Long lost description', 'ui', NULL, '{}', 'published', false, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('aa790000-0000-4000-8000-000000000006', 'aa780000-0000-4000-8000-000000000005', 'wasm-tool', 'Wasm Tool', 'Runs', 'Runs in the runner', 'skill', NULL, '{}', 'draft', false, '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');

-- alpha-notes: 1.0.0, 1.1.0 (latest approved) and 2.0.0 (older publish date, new
-- required field), plus a draft. draft-thing: a draft and a validation failure.
INSERT INTO marketplace_versions (id, listing_id, version, status, runtime_kind, permissions, network_hosts, config_fields, changelog, artifact_key, artifact_sha256, artifact_signature, manifest_json, published_at, created_at, updated_at)
VALUES
  ('aa7a0000-0000-4000-8000-000000000001', 'aa790000-0000-4000-8000-000000000001', '1.0.0', 'approved', 'declarative', '{memory.read}', '{}', '[{"key":"api_key","label":"API key","kind":"secret","isRequired":true},{"key":"region","label":"Region","placeholder":"eu","kind":"text","isRequired":false}]', 'First', NULL, NULL, NULL, NULL, '2026-02-01T00:00:00Z', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000002', 'aa790000-0000-4000-8000-000000000001', '1.1.0', 'approved', 'declarative', '{memory.read,vault.read}', '{}', '[{"key":"api_key","label":"API key","kind":"secret","isRequired":true},{"key":"region","label":"Region","placeholder":"eu","kind":"text","isRequired":false}]', 'Second', NULL, NULL, NULL, NULL, '2026-03-01T00:00:00Z', '2026-01-07T00:00:00Z', '2026-01-07T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000003', 'aa790000-0000-4000-8000-000000000001', '2.0.0', 'approved', 'declarative', '{}', '{}', '[{"key":"new_required","label":"New","kind":"url","isRequired":true}]', NULL, NULL, NULL, NULL, NULL, '2026-01-15T00:00:00Z', '2026-01-08T00:00:00Z', '2026-01-08T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000004', 'aa790000-0000-4000-8000-000000000001', '3.0.0-draft', 'draft', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-09T00:00:00Z', '2026-01-09T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000005', 'aa790000-0000-4000-8000-000000000002', '1.0.0', 'approved', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, '2026-02-01T00:00:00Z', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000006', 'aa790000-0000-4000-8000-000000000003', '0.1.0', 'draft', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000007', 'aa790000-0000-4000-8000-000000000003', 'not-semver', 'draft', 'wasm', '{}', '{example.parity.test}', '[]', NULL, NULL, NULL, NULL, '\x7b226e6f7065223a747275657d', NULL, '2026-01-07T00:00:00Z', '2026-01-07T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000008', 'aa790000-0000-4000-8000-000000000003', '0.2.0', 'in_review', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, NULL, '2026-01-08T00:00:00Z', '2026-01-08T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000009', 'aa790000-0000-4000-8000-000000000004', '1.0.0', 'approved', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, '2026-02-01T00:00:00Z', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-00000000000a', 'aa790000-0000-4000-8000-000000000004', '1.1.0', 'approved', 'declarative', '{}', '{}', '[]', NULL, NULL, NULL, NULL, NULL, '2026-02-02T00:00:00Z', '2026-01-07T00:00:00Z', '2026-01-07T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-00000000000b', 'aa790000-0000-4000-8000-000000000005', '1.0.0', 'approved', 'wasm', '{}', '{}', '[]', NULL, 'other-pub/lost-wasm/gone.wasm', '0000000000000000000000000000000000000000000000000000000000000000', '0000000000000000000000000000000000000000000000000000000000000000', '\x7b22736368656d6156657273696f6e223a312c22746f6f6c73223a5b7b226e616d65223a226563686f222c226465736372697074696f6e223a2245636f227d5d7d', '2026-02-01T00:00:00Z', '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-00000000000c', 'aa790000-0000-4000-8000-000000000005', '1.1.0', 'approved', 'wasm', '{}', '{}', '[]', NULL, '../escape.wasm', '0000000000000000000000000000000000000000000000000000000000000000', '0000000000000000000000000000000000000000000000000000000000000000', NULL, '2026-01-01T00:00:00Z', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');

-- wasm-tool: scenario 04 uploads the artifact (AGFzbQEAAAA=), submits and approves
-- 1.0.0, and scenario 07 runs it. The signature is HMAC-SHA256 of "<key>:<digest>"
-- with the parity signing key.
INSERT INTO marketplace_versions (id, listing_id, version, status, runtime_kind, permissions, network_hosts, config_fields, artifact_key, artifact_sha256, artifact_signature, manifest_json, created_at, updated_at)
VALUES
  ('aa7a0000-0000-4000-8000-000000000010', 'aa790000-0000-4000-8000-000000000006', '1.0.0', 'draft', 'wasm', '{memory.read,memory.write,network.fetch,output.emit,vault.read,vault.write}', '{allowed.parity.test}', '[]', 'wasm-pub/wasm-tool/93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476.wasm', '93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476', '68b4d6f32ceffdc14310cdd65d55677ea5a1fe44befafe1d42f169d99b394ee0', convert_to('{"schemaVersion":1,"tools":[{"name":"echo","description":"echo tool"},{"name":"capable","description":"capable tool"},{"name":"loop","description":"loop tool"},{"name":"emptyreq","description":"emptyreq tool"},{"name":"badreq","description":"badreq tool"},{"name":"reject","description":"reject tool"},{"name":"garbage","description":"garbage tool"},{"name":"partial","description":"partial tool"},{"name":"unknown-to-runner","description":"unknown-to-runner tool"}]}', 'UTF8'), '2026-01-06T00:00:00Z', '2026-01-06T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000011', 'aa790000-0000-4000-8000-000000000006', '1.0.1', 'draft', 'wasm', '{}', '{}', '[]', 'wasm-pub/wasm-tool/93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476.wasm', '93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476', '68b4d6f32ceffdc14310cdd65d55677ea5a1fe44befafe1d42f169d99b394ee0', convert_to('{"schemaVersion":2,"tools":[]}', 'UTF8'), '2026-01-07T00:00:00Z', '2026-01-07T00:00:00Z'),
  ('aa7a0000-0000-4000-8000-000000000012', 'aa790000-0000-4000-8000-000000000006', '1.0.2', 'draft', 'wasm', '{}', '{}', '[]', 'wasm-pub/wasm-tool/93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476.wasm', '93a44bbb96c751218e4c00d479e4c14358122a389acca16205b1e4d0dc5f9476', '68b4d6f32ceffdc14310cdd65d55677ea5a1fe44befafe1d42f169d99b394ee0', convert_to('{"schemaVersion":1,"tools":[{"name":"Bad Name","description":"d"},{"name":"Bad Name","description":"d"}]}', 'UTF8'), '2026-01-08T00:00:00Z', '2026-01-08T00:00:00Z');

INSERT INTO marketplace_submissions (id, version_id, publisher_user_id, status, validation_errors, review_note, reviewed_by_user_id, submitted_at, reviewed_at, created_at, updated_at)
VALUES
  ('aa7b0000-0000-4000-8000-000000000001', 'aa7a0000-0000-4000-8000-000000000008', 'aa770000-0000-4000-8000-0000000000a2', 'in_review', '{}', NULL, NULL, '2026-01-08T01:00:00Z', NULL, '2026-01-08T01:00:00Z', '2026-01-08T01:00:00Z'),
  ('aa7b0000-0000-4000-8000-000000000002', 'aa7a0000-0000-4000-8000-000000000006', 'aa770000-0000-4000-8000-0000000000a2', 'draft', '{invalid_semver}', NULL, NULL, NULL, NULL, '2026-01-06T01:00:00Z', '2026-01-06T01:00:00Z');

-- Ratings: one hidden by moderation, which reviews and the average skip.
INSERT INTO marketplace_ratings (id, listing_id, user_id, rating, body, moderation_status, created_at, updated_at)
VALUES
  ('aa7b1000-0000-4000-8000-000000000001', 'aa790000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000ad', 5, 'Great', 'published', '2026-02-02T00:00:00Z', '2026-02-02T00:00:00Z'),
  ('aa7b1000-0000-4000-8000-000000000002', 'aa790000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000ae', 2, NULL, 'published', '2026-02-03T00:00:00Z', '2026-02-03T00:00:00Z'),
  ('aa7b1000-0000-4000-8000-000000000003', 'aa790000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000af', 1, 'Rude', 'hidden', '2026-02-04T00:00:00Z', '2026-02-04T00:00:00Z');

-- Installs. Ciphertext bytes that do not open (a failure path, seeded because a
-- real one depends on the sealing nonce): mkt_bad's upgrade and install are 500s.
-- mkt_run has a disabled install and two on versions that cannot run.
INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, marketplace_version_id, granted_permissions, created_at, updated_at)
VALUES
  ('aa7b2000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000b3', 'alpha-notes', 'enabled', '\x00', '\x00', 'aa7a0000-0000-4000-8000-000000000001', '{memory.read}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z'),
  ('aa7b2000-0000-4000-8000-000000000002', 'aa770000-0000-4000-8000-0000000000a9', 'beta-tools', 'disabled', '\x00', '\x00', 'aa7a0000-0000-4000-8000-000000000005', '{}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z'),
  ('aa7b2000-0000-4000-8000-000000000003', 'aa770000-0000-4000-8000-0000000000a9', 'revoke-me', 'enabled', '\x00', '\x00', 'aa7a0000-0000-4000-8000-000000000009', '{}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z'),
  ('aa7b2000-0000-4000-8000-000000000004', 'aa770000-0000-4000-8000-0000000000a9', 'lost-wasm', 'enabled', '\x00', '\x00', 'aa7a0000-0000-4000-8000-00000000000b', '{}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z'),
  ('aa7b2000-0000-4000-8000-000000000005', 'aa770000-0000-4000-8000-0000000000aa', 'lost-wasm', 'enabled', '\x00', '\x00', 'aa7a0000-0000-4000-8000-00000000000c', '{}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z');

-- A memory for the memory.read capability; mkt_cap owns it, bob does not.
INSERT INTO memories (id, tenant_id, content, tags, created_at, updated_at)
VALUES
  ('aa7b3000-0000-4000-8000-000000000001', 'aa770000-0000-4000-8000-0000000000aa', 'A remembered thing', '{one,two}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z'),
  ('aa7b3000-0000-4000-8000-000000000002', 'b0b00000-0000-4000-8000-000000000002', 'Bob''s private thing', '{}', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z');

-- Snapshot views: sealing a config uses a random nonce, so compare the plaintext's
-- length (ciphertext length) instead of the bytes.
CREATE VIEW parity_plugin_installs AS
SELECT id, tenant_id, plugin_slug, status, length(config_ciphertext) AS ciphertext_len, length(config_nonce) AS nonce_len,
       last_sync_at, marketplace_version_id, granted_permissions, created_at, updated_at
FROM plugin_installs;

-- duration_ms counts whole seconds of real time.
CREATE VIEW parity_marketplace_executions AS
SELECT id, tenant_id, install_id, version_id, tool_name, status, error_code, duration_ms IS NOT NULL AS finished, created_at
FROM marketplace_executions;
