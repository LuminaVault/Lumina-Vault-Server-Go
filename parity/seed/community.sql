-- Community parity seed (parity/scenarios/community). Ids use the c0aa (users),
-- c0ab (profiles) and c0ac (posts) prefixes. Own users: communityWriteByUser is
-- 10/min per user and every write route, rejected or not, takes a token.
-- comm_mod, comm_mod2 and comm_mod3 are moderators (users.is_admin).
-- Times relative to now() are evaluated once when the template is seeded.

INSERT INTO users (id, email, password_hash, is_verified, username, is_admin, created_at, updated_at)
VALUES
  ('c0aa0000-0000-4000-8000-0000000000a1', 'comm_ann@parity.test', 'x', true, 'comm_ann', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a2', 'comm_ben@parity.test', 'x', true, 'comm_ben', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a3', 'comm_cid@parity.test', 'x', true, 'comm_cid', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a4', 'comm_dee@parity.test', 'x', true, 'comm_dee', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a5', 'comm_mod@parity.test', 'x', true, 'comm_mod', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a6', 'comm_old@parity.test', 'x', true, 'comm_old', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a7', 'comm_sus@parity.test', 'x', true, 'comm_sus', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a8', 'comm_cap@parity.test', 'x', true, 'comm_cap', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000a9', 'comm_rep1@parity.test', 'x', true, 'comm_rep1', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000aa', 'comm_rep2@parity.test', 'x', true, 'comm_rep2', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000ab', 'comm_rep3@parity.test', 'x', true, 'comm_rep3', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000ac', 'comm_new@parity.test', 'x', true, 'comm_new', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000ad', 'comm_val@parity.test', 'x', true, 'comm_val', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000ae', 'comm_val2@parity.test', 'x', true, 'comm_val2', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000af', 'comm_blk@parity.test', 'x', true, 'comm_blk', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b0', 'comm_post@parity.test', 'x', true, 'comm_post', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b1', 'comm_lim@parity.test', 'x', true, 'comm_lim', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b2', 'comm_read@parity.test', 'x', true, 'comm_read', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b3', 'comm_page@parity.test', 'x', true, 'comm_page', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b4', 'comm_mod2@parity.test', 'x', true, 'comm_mod2', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b5', 'comm_cap2@parity.test', 'x', true, 'comm_cap2', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b6', 'comm_new2@parity.test', 'x', true, 'comm_new2', false, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('c0aa0000-0000-4000-8000-0000000000b7', 'comm_mod3@parity.test', 'x', true, 'comm_mod3', true, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
SELECT id, id, username || '''s Vault', created_at, updated_at FROM users WHERE id::text LIKE 'c0aa0000-%';

-- comm_dee, comm_new, comm_new2, comm_val2, comm_lim and comm_mod2 have no profile.
-- comm_old accepted an older version of the terms; comm_sus is suspended.
INSERT INTO community_profiles (id, user_id, handle, display_name, terms_version, terms_accepted_at, posting_suspended_until, created_at, updated_at)
VALUES
  ('c0ab0000-0000-4000-8000-0000000000a1', 'c0aa0000-0000-4000-8000-0000000000a1', 'ann_h', 'Display Ann', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a2', 'c0aa0000-0000-4000-8000-0000000000a2', 'ben_h', 'Display Ben', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a3', 'c0aa0000-0000-4000-8000-0000000000a3', 'cid_h', 'Display Cid', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a5', 'c0aa0000-0000-4000-8000-0000000000a5', 'mod_h', 'Display Mod', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a6', 'c0aa0000-0000-4000-8000-0000000000a6', 'old_h', 'Display Old', 0, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a7', 'c0aa0000-0000-4000-8000-0000000000a7', 'sus_h', 'Display Sus', 1, '2026-01-02T00:00:00Z', '2099-01-01T00:00:00Z', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a8', 'c0aa0000-0000-4000-8000-0000000000a8', 'cap_h', 'Display Cap', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000a9', 'c0aa0000-0000-4000-8000-0000000000a9', 'rep1_h', 'Display Rep1', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000aa', 'c0aa0000-0000-4000-8000-0000000000aa', 'rep2_h', 'Display Rep2', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000ab', 'c0aa0000-0000-4000-8000-0000000000ab', 'rep3_h', 'Display Rep3', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000ad', 'c0aa0000-0000-4000-8000-0000000000ad', 'val_h', 'Display Val', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000af', 'c0aa0000-0000-4000-8000-0000000000af', 'blk_h', 'Display Blk', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000b0', 'c0aa0000-0000-4000-8000-0000000000b0', 'post_h', 'Display Post', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000b2', 'c0aa0000-0000-4000-8000-0000000000b2', 'read_h', 'Display Read', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000b3', 'c0aa0000-0000-4000-8000-0000000000b3', 'page_h', 'Display Page', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000b5', 'c0aa0000-0000-4000-8000-0000000000b5', 'cap2_h', 'Display Cap2', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z'),
  ('c0ab0000-0000-4000-8000-0000000000b7', 'c0aa0000-0000-4000-8000-0000000000b7', 'mod3_h', 'Display Mod3', 1, '2026-01-02T00:00:00Z', NULL, '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

INSERT INTO community_posts (id, parent_id, author_kind, author_profile_id, title, body, status, moderation_category, hidden_reason, reply_count, last_activity_at, created_at, updated_at)
VALUES
  ('c0ac0000-0000-4000-8000-000000000001', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Morning walks', 'Walking before coffee helps.', 'visible', NULL, NULL, 2, '2026-03-05T10:00:00.123456Z', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000002', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a2', 'Reading lists', 'What are you reading?', 'visible', NULL, NULL, 0, '2026-03-04T10:00:00Z', '2026-03-02T00:00:00Z', '2026-03-02T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000003', NULL, 'ai', NULL, 'Daily prompt', 'What small thing went well?', 'visible', NULL, NULL, 0, '2026-03-03T10:00:00Z', '2026-01-05T12:00:00Z', '2026-01-05T12:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000004', 'c0ac0000-0000-4000-8000-000000000001', 'user', 'c0ab0000-0000-4000-8000-0000000000a2', NULL, 'Same here.', 'visible', NULL, NULL, 0, '2026-03-05T09:00:00Z', '2026-03-05T09:00:00Z', '2026-03-05T09:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000005', 'c0ac0000-0000-4000-8000-000000000001', 'user', 'c0ab0000-0000-4000-8000-0000000000a3', NULL, 'I walk after lunch.', 'visible', NULL, NULL, 0, '2026-03-05T10:00:00.123456Z', '2026-03-05T10:00:00.123456Z', '2026-03-05T10:00:00.123456Z'),
  ('c0ac0000-0000-4000-8000-000000000006', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a3', 'Hidden thread', 'Reported into hiding.', 'hidden', NULL, 'reports', 0, '2026-02-20T00:00:00Z', '2026-02-20T00:00:00Z', '2026-02-20T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000007', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Pending thread', 'Waiting for moderation.', 'pending', NULL, NULL, 0, '2026-02-21T00:00:00Z', '2026-02-21T00:00:00Z', '2026-02-21T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000008', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a2', 'Rejected thread', 'Turned down.', 'rejected', 'spam', NULL, 0, '2026-02-22T00:00:00Z', '2026-02-22T00:00:00Z', '2026-02-22T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000009', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a2', 'Deleted thread', 'Gone.', 'deleted', NULL, NULL, 0, '2026-02-23T00:00:00Z', '2026-02-23T00:00:00Z', '2026-02-23T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000a', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a3', 'Report target', 'Report me.', 'visible', NULL, NULL, 0, '2026-02-24T00:00:00Z', '2026-02-24T00:00:00Z', '2026-02-24T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000b', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'To be hidden', 'A moderator hides this.', 'visible', NULL, NULL, 1, '2026-02-25T00:00:00Z', '2026-02-25T00:00:00Z', '2026-02-25T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000c', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'To be restored', 'A moderator restores this.', 'hidden', NULL, 'reports', 0, '2026-02-26T00:00:00Z', '2026-02-26T00:00:00Z', '2026-02-26T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000d', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'To be removed', 'A moderator removes this.', 'visible', NULL, NULL, 0, '2026-02-27T00:00:00Z', '2026-02-27T00:00:00Z', '2026-02-27T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000e', 'c0ac0000-0000-4000-8000-00000000000b', 'user', 'c0ab0000-0000-4000-8000-0000000000a2', NULL, 'A reply on the hidden thread.', 'visible', NULL, NULL, 0, '2026-02-25T01:00:00Z', '2026-02-25T01:00:00Z', '2026-02-25T01:00:00Z'),
  ('c0ac0000-0000-4000-8000-00000000000f', 'c0ac0000-0000-4000-8000-000000000001', 'user', 'c0ab0000-0000-4000-8000-0000000000a3', NULL, 'Pending reply.', 'pending', NULL, NULL, 0, '2026-03-05T11:00:00Z', '2026-03-05T11:00:00Z', '2026-03-05T11:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000010', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000b0', 'Delete my thread', 'Deleted by its author.', 'visible', NULL, NULL, 1, '2026-01-20T00:00:00Z', '2026-01-20T00:00:00Z', '2026-01-20T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000011', 'c0ac0000-0000-4000-8000-000000000010', 'user', 'c0ab0000-0000-4000-8000-0000000000b0', NULL, 'Delete my reply.', 'visible', NULL, NULL, 0, '2026-01-20T01:00:00Z', '2026-01-20T01:00:00Z', '2026-01-20T01:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000012', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000a2', NULL, 'A reply cannot have replies.', 'visible', NULL, NULL, 0, '2026-03-02T01:00:00Z', '2026-03-02T01:00:00Z', '2026-03-02T01:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000020', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Tied thread 0', 'Same moment.', 'visible', NULL, NULL, 0, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000021', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Tied thread 1', 'Same moment.', 'visible', NULL, NULL, 0, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000022', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Tied thread 2', 'Same moment.', 'visible', NULL, NULL, 0, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000023', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Tied thread 3', 'Same moment.', 'visible', NULL, NULL, 0, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000024', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a1', 'Tied thread 4', 'Same moment.', 'visible', NULL, NULL, 0, '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z', '2026-01-10T00:00:00Z'),
  ('c0ac0000-0000-4000-8000-000000000030', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 0', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000031', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 1', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000032', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 2', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000033', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 3', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000034', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 4', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000035', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 5', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000036', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 6', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000037', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 7', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000038', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 8', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000039', NULL, 'user', 'c0ab0000-0000-4000-8000-0000000000a8', 'Cap thread 9', 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000040', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000041', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000042', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000043', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000044', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000045', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000046', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000047', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000048', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000049', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004a', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004b', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004c', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004d', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004e', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000004f', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000050', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000051', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000052', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000053', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000054', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000055', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000056', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000057', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000058', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000059', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005a', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005b', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005c', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005d', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005e', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000005f', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000060', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000061', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000062', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000063', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000064', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000065', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000066', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000067', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000068', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000069', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006a', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006b', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006c', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006d', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006e', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000006f', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000070', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000071', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000072', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000073', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000074', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000075', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000076', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000077', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000078', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-000000000079', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000007a', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour'),
  ('c0ac0000-0000-4000-8000-00000000007b', 'c0ac0000-0000-4000-8000-000000000002', 'user', 'c0ab0000-0000-4000-8000-0000000000b5', NULL, 'Counted.', 'pending', NULL, NULL, 0, now() - interval '1 hour', now() - interval '1 hour', now() - interval '1 hour');

-- A past daily post: isDailyPost and the AI label on the board. Today has no row
-- in this seed (GET /daily/today is 204 and the admin run claims it).
INSERT INTO community_daily_posts (day, status, attempts, post_id, source_kind, source_urls, updated_at)
VALUES ('2026-01-05', 'published', 1, 'c0ac0000-0000-4000-8000-000000000003', 'curated', '[]', '2026-01-05T12:00:00Z');

-- Two open reports on the restored thread and a resolved one on the report target.
INSERT INTO community_reports (id, post_id, reporter_profile_id, reason, note, status, created_at)
VALUES
  ('c0ad0000-0000-4000-8000-000000000001', 'c0ac0000-0000-4000-8000-00000000000c', 'c0ab0000-0000-4000-8000-0000000000a9', 'spam', 'first', 'open', '2026-02-26T01:00:00Z'),
  ('c0ad0000-0000-4000-8000-000000000002', 'c0ac0000-0000-4000-8000-00000000000c', 'c0ab0000-0000-4000-8000-0000000000aa', 'hate', NULL, 'open', '2026-02-26T02:00:00Z'),
  ('c0ad0000-0000-4000-8000-000000000003', 'c0ac0000-0000-4000-8000-00000000000a', 'c0ab0000-0000-4000-8000-0000000000ab', 'other', NULL, 'dismissed', '2026-02-24T02:00:00Z');

INSERT INTO community_blocks (blocker_profile_id, blocked_profile_id, created_at)
VALUES ('c0ab0000-0000-4000-8000-0000000000af', 'c0ab0000-0000-4000-8000-0000000000a3', '2026-02-01T00:00:00Z');

-- Snapshot view: last_error is the failure's text, which names each server's own error.
CREATE VIEW parity_community_daily AS
SELECT day, status, attempts, post_id, theme_id, topic_key, source_kind, source_urls, model, last_error IS NOT NULL AS has_error, claimed_at, updated_at
FROM community_daily_posts;
