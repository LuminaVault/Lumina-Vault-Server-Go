-- News ticker fixture (parity/scenarios/news). One user per install state so
-- each settingsByUser bucket (30/min, shared with /v1/plugins) and each
-- 60 s ticker cache entry belongs to a single scenario step group.
-- Feed URLs drive the stub aggregator (parity/stubs/feeds).
--
-- config_ciphertext/config_nonce are SecretBox-sealed with the parity master
-- key (32 bytes of 0x01) for the row's own tenant, except news_eve's, which
-- is sealed for news_amy and therefore does not open. Plaintext configs:
--   news_amy   feed_urls " https://feeds.parity.test/amy.json, http://10.0.0.1/private.json,https://feeds.parity.test/amy.json , not a url\nhttps://feeds.parity.test/stale.json,https://news.parity.test/world.json,http://localhost/x.json"
--   news_dan   (disabled) feed_urls https://feeds.parity.test/dan.json
--   news_ned   no install
--   news_eve   feed_urls https://feeds.parity.test/eve.json (sealed for amy)
--   news_err/bad/hup/nf/mk/nox/nog  one feed each: error, malformed, hangup,
--              notfound, missingkey, noext, nogenerated (.json)
--   news_probe 17 comma-separated entries: case, space, "+", "&", nip.io,
--              fdic.gov, IPv6, CRLF, non-ASCII, ftp, then p12..p17 (cap 10)
--   news_nul   {"other":"x"} (no feed_urls: curated feeds only)

INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
VALUES
  ('a0e00000-0000-4000-8000-0000000000a1', 'news_amy@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_amy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000b2', 'news_dan@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_dan', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000c3', 'news_ned@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_ned', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000d4', 'news_eve@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_eve', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000e5', 'news_err@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_err', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000f6', 'news_bad@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_bad', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000a7', 'news_hup@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_hup', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000b8', 'news_nf@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_nf', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000c9', 'news_mk@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_mk', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000da', 'news_nox@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_nox', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000eb', 'news_nog@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_nog', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000fc', 'news_probe@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_probe', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
  ('a0e00000-0000-4000-8000-0000000000ad', 'news_nul@parity.test', '$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau', true, 'news_nul', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce, created_at, updated_at)
VALUES
  ('9e000000-0000-4000-8000-000000000001', 'a0e00000-0000-4000-8000-0000000000a1', 'news-ticker', 'enabled', '\x0b07f32d7f231e7146a1bc84385081735b16dacb34d61f0d45931d8f67872004778fba18532c8517873ec40f6fd3e937e659d536f4659561568e66b8320222de84029e7b0a5cf883b5e2b9545d5ae75006752403ccd34552356813fb7039d1a21ee5bef617dad580e2afca152ca596aa68dfda5c9913dc0f9e1249def323aeba3e584db5cd04f0e565ae04a37d746736da25af19e1ac5a306588559aac78dd0277eeb92083303ffb4815f4c368e920e1d64ed45f481f03211c228f9f44fea6da9c0cf694b6d82b022073d2df9ebac2235ec47aae0817e1ac00e77f6a96c56d3873f0d63644707f43b001c9a951c44e88eb48', '\xc9ab1a5a1329046840ae23aa', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000002', 'a0e00000-0000-4000-8000-0000000000b2', 'news-ticker', 'disabled', '\xbe11a3ca04ab472efe52dc2115fb7e938ac0573f169fe9980a13c396559678b9d0160aadf870536d36c723f01d42e6b3eb9faaeec4baa02e9bfb57b3e4e7c4b47434', '\xdb19192630bdbc111ef89770', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000003', 'a0e00000-0000-4000-8000-0000000000d4', 'news-ticker', 'enabled', '\x962004d4b3157a88081b68f8041f3b3bceef294ba0912d5a2f63f3d4320304cdff62d3a1e6e08cb7a798b51299cf60da86cd14fa39709a35952d28a803e0bdaaeef2', '\x446bcfa60a88be19f94eb170', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000004', 'a0e00000-0000-4000-8000-0000000000e5', 'news-ticker', 'enabled', '\x1ff8529565c821535b2d7495eace8a68b365f676160dd2f0c70e579a24339b5072392fe5394dd74bee4bf8870b1865c48462a38236b579d792beb01125707985a6982eba', '\x61441d62472e095da5961f20', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000005', 'a0e00000-0000-4000-8000-0000000000f6', 'news-ticker', 'enabled', '\x1234607ad9de2e19ccb5036846cd5eda421d3c59f729d289a4a02bd947515384b10954a4f940bc90a572a8907897e1ab30e95d35a75166b05ded249d93d210be58c94a3f46f15064', '\xa38f44716e633856a56f5187', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000006', 'a0e00000-0000-4000-8000-0000000000a7', 'news-ticker', 'enabled', '\x7cdfb9dca1d039586032b618a13ca18a2f1e7bc255204c8959c2d278fdff49d459f13a7c8e5946d6fd100dc4860ab46c9cd21d3e5c7bf37977dcc17fe5f9ddaa0b14dd471e', '\xed582f4502b7a5cdd3f30971', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000007', 'a0e00000-0000-4000-8000-0000000000b8', 'news-ticker', 'enabled', '\x253f4e9269dee037a0ee70161610aa367ea9fdf52a34e00cea5e1bd37d95720f180a312ac8a7b1ee16dadbdfd6eb19842fadc5833f83ef78d8c5e3afb10a7da0f1b6a0b7583af9', '\x7a3c6b7286640380a96de61c', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000008', 'a0e00000-0000-4000-8000-0000000000c9', 'news-ticker', 'enabled', '\xc3b15e67b70b7fb06a9e57a0ecb86adc4748ac61144754f6f365e497d2cf7a78d8ac0c6db9a58248e9e4d1caf8378a4f61b8decc4400d8aa240a8a0e4ad71e769106f6cc7c632fcc71', '\x5c8b0d25d038a966751c0945', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-000000000009', 'a0e00000-0000-4000-8000-0000000000da', 'news-ticker', 'enabled', '\xe340b5cf9fedf1c4047fe75fead2f1850cf3bf1ad37a58333f2e41f1c563d6f590bab00b4bdcfc4431dadf9ab412d9b5cc0cfd40527d7d2c85bdd853084e4f780d42ce5a', '\xd002d115659308817ea3390e', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-00000000000a', 'a0e00000-0000-4000-8000-0000000000eb', 'news-ticker', 'enabled', '\xb8cee13cd388316d5b659a578c622a050c7fe2228dfe9f177db6f26a2e4c89afbc7ae0d684a8f7ef90274b9039453072befba30e02f45183142c9abf538e5521b99cc20483242f3bdebc', '\x4986162d91db8b32d4bbb4b4', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-00000000000b', 'a0e00000-0000-4000-8000-0000000000fc', 'news-ticker', 'enabled', '\xd0b64c1c7cb75c04444fb0690b02911314d0a041010e4bebc241cbfb1a7b02d1f66837d4e0211c0127b5403e57997b34e396c1023797820c3cc771343119b8d087a668fe8a4b17ba8d70c30d7566c22a8d966ae2cabc5a8e9a5a11cadd1323ca31e04846250bb8871ae241fb6598a084addb5f950d0eb67fe0faa98223092877abf87868265215a6e6025118f84ab42165d0f1bd2ccecbcca5c3d243337c9c93a8b13ba0eedb57473932b60db043fa7f36dd7ec3db5d12ac0073c9815d7887d11a09f21957413bf03aa940f7a719ea98c45a68f51f135fd2e609a49ae71dc3401e25ecc791f2c8b9c5f2ef7ee5679a2f3a49b919fad60a3b6ffc3be8a5980b1cb26b0c27dc672560a7509627f9d422cc4bf8827b18cba58c5488ffd95876214658fcf902abcb20b8885666c29d4ded96acbef51b7821c6ca9afd4cee1e74a715e3f2f1c0501a187eb086051853c5e8403f3503618d0a4e036aa41a1ead696f582c5cac7a5126c90536984fe01abc334778f28141358652e09b706bfe8f92ed32fde4e1c1ae9d921c58aa49e40d7874ed619224b087c89e25e291bc15868b2db53cbac2889fe8d6510b52afd2015a898236bb241795aea9e8a653a00546d3960d13f94d155f8dbea20e05978441f6190e47d5d879ccd18b9d081a9515c282bac4516102af46118893b35787024f67994c92ae20ef466d5ee847bff800369748b0c00862b51001eb78f93cb4db2c10e79064b056c8a7abcffb57adcb55e599bb0f8c9cd836c9b07762d663dd86b67b4e07449f1eb5c648d87e1c67e9e20496e2abd3a5f16ff981709813cec6b40c835a418ba90beac195ef9eba8766b4bc44b56054de8deb31d35b59b3f88d70c4b5c526cc5235e04f2d027242b97e61cc78f7580d153d57f052069eab87c48f9d7a016193debf4bc9eb89f0be9714f0ba10062a9f', '\x818f8afda2035a2713320f10', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z'),
  ('9e000000-0000-4000-8000-00000000000c', 'a0e00000-0000-4000-8000-0000000000ad', 'news-ticker', 'enabled', '\xc5a02058635fa0950734783597fd8ccd1ea7d29c5c6a64a8fbc1bb8a5a', '\x857e0b95a63be298247e0ed9', '2026-01-05T00:00:00Z', '2026-01-05T00:00:00Z');
