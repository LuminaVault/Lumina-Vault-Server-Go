-- Today's daily post already published: /daily/today has it and the admin run is a no-op.
INSERT INTO community_posts (id, author_kind, title, body, status, moderated_at, last_activity_at, created_at, updated_at)
VALUES ('c0ac0000-0000-4000-8000-0000000000fe', 'ai', 'Today in small habits', 'One thing that helped today.', 'visible', now(), now(), now(), now());
INSERT INTO community_daily_posts (day, status, attempts, post_id, theme_id, topic_key, source_kind, source_urls, model, claimed_at, updated_at)
VALUES ((now() AT TIME ZONE 'utc')::date, 'published', 1, 'c0ac0000-0000-4000-8000-0000000000fe', 'habits', 'habits/small', 'curated', '[]', 'parity-model', now(), now());
