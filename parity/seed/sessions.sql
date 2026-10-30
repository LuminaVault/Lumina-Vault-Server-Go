-- Sessions (Swift: Sources/App/Sessions/SessionsController.swift). Loaded after
-- base.sql. GET /v1/sessions only reads conversations + conversation_messages.
-- Every updated_at and message created_at is distinct so ORDER BY ... LIMIT 1
-- is deterministic. The previews probe Swift's `String.prefix(140)`, which
-- counts grapheme clusters, not bytes or code points.

-- A space of alice's for ?workspace=. The slug is unusual on purpose so it
-- cannot collide with rows the spaces scenarios create.
INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at)
VALUES ('5ace0000-0000-4000-8000-0000000000b1', 'a11ce000-0000-4000-8000-000000000001', 'Sessions WS', 'parity-sessions-ws', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO conversations (id, tenant_id, title, space_id, created_at, updated_at)
VALUES
  ('c0000000-0000-4000-8000-0000000000c1', 'a11ce000-0000-4000-8000-000000000001', 'Long ASCII', NULL, '2026-02-01T00:00:00Z', '2026-02-07T00:00:00Z'),
  ('c0000000-0000-4000-8000-0000000000c2', 'a11ce000-0000-4000-8000-000000000001', 'Combining mark', '5ace0000-0000-4000-8000-0000000000b1', '2026-02-01T00:00:00Z', '2026-02-06T12:30:45.5Z'),
  ('c0000000-0000-4000-8000-0000000000c3', 'a11ce000-0000-4000-8000-000000000001', 'No messages', NULL, '2026-02-01T00:00:00Z', '2026-02-05T00:00:00Z'),
  ('c0000000-0000-4000-8000-0000000000c4', 'a11ce000-0000-4000-8000-000000000001', 'Short', '5ace0000-0000-4000-8000-0000000000b1', '2026-02-01T00:00:00Z', '2026-02-04T00:00:00Z'),
  ('c0000000-0000-4000-8000-0000000000c5', 'a11ce000-0000-4000-8000-000000000001', 'Emoji', NULL, '2026-02-01T00:00:00Z', '2026-02-03T00:00:00Z'),
  ('c0000000-0000-4000-8000-0000000000c6', 'a11ce000-0000-4000-8000-000000000001', 'CRLF', NULL, '2026-02-01T00:00:00Z', '2026-02-02T00:00:00Z'),
  ('c0000000-0000-4000-8000-0000000000c7', 'b0b00000-0000-4000-8000-000000000002', 'Bob chat', NULL, '2026-02-01T00:00:00Z', '2026-02-08T00:00:00Z');

INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
VALUES
  ('d0000000-0000-4000-8000-0000000000d1', 'c0000000-0000-4000-8000-0000000000c1', 'user', 'first', '2026-02-07T00:00:01Z'),
  ('d0000000-0000-4000-8000-0000000000d2', 'c0000000-0000-4000-8000-0000000000c1', 'assistant', 'second', '2026-02-07T00:00:02Z'),
  -- latest by created_at although inserted before an older row below
  ('d0000000-0000-4000-8000-0000000000d3', 'c0000000-0000-4000-8000-0000000000c1', 'user', repeat('0123456789', 20), '2026-02-07T00:00:04Z'),
  ('d0000000-0000-4000-8000-0000000000d4', 'c0000000-0000-4000-8000-0000000000c1', 'assistant', 'older third', '2026-02-07T00:00:03Z'),
  -- 139 'a' + e + U+0301 (one grapheme) + 'xyz': the accent is kept, 'x' is cut
  ('d0000000-0000-4000-8000-0000000000d5', 'c0000000-0000-4000-8000-0000000000c2', 'user', repeat('a', 139) || 'e' || chr(769) || 'xyz', '2026-02-06T00:00:01Z'),
  ('d0000000-0000-4000-8000-0000000000d6', 'c0000000-0000-4000-8000-0000000000c4', 'user', 'older', '2026-02-04T00:00:01Z'),
  ('d0000000-0000-4000-8000-0000000000d7', 'c0000000-0000-4000-8000-0000000000c4', 'assistant', 'hi there', '2026-02-04T00:00:02Z'),
  -- 137 'b' + ZWJ family (one grapheme) + flag (one grapheme) + skin-tone thumb (one) + 'z'
  ('d0000000-0000-4000-8000-0000000000d8', 'c0000000-0000-4000-8000-0000000000c5', 'user',
   repeat('b', 137) || chr(128104) || chr(8205) || chr(128105) || chr(8205) || chr(128103) || chr(127477) || chr(127481) || chr(128077) || chr(127997) || 'z',
   '2026-02-03T00:00:01Z'),
  -- 139 'c' + CRLF (one grapheme) + 'tail'
  ('d0000000-0000-4000-8000-0000000000d9', 'c0000000-0000-4000-8000-0000000000c6', 'user', repeat('c', 139) || chr(13) || chr(10) || 'tail', '2026-02-02T00:00:01Z'),
  ('d0000000-0000-4000-8000-0000000000da', 'c0000000-0000-4000-8000-0000000000c7', 'user', 'bob says hi', '2026-02-08T00:00:01Z');
