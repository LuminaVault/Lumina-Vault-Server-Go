# Bundled Hermes Skills (HER-276)

LuminaVault's `kb-*` skills (`kb-compile`, `kb-ingest`, `kb-import`,
`kb-merge-vault`, `kb-output`) vendored at `LuminaVaultServer/Sources/App/Resources/HermesSkills/`
so every deployment ships them without depending on the host's
`~/.hermes/skills/` tree.

The same directory is a SwiftPM resource (`.copy("Resources/HermesSkills")` in
`Package.swift`): `HermesBundledSkills` reads it at runtime so the Hermes Mirror
can install the `kb-*` skills onto a user's own Hermes when it creates a vault
there. One tree, two consumers — edit here only.

## How it works

`docker/hermes.Dockerfile` extends the digest-pinned `LuminaVaultHermesAgent`
image from GHCR. That fork owns the `/v1/ingestions` API and its advertised
remote-source capability fields; this server layer adds LuminaVault skills,
Mnemosyne, and the runtime entrypoint. It `COPY`s this directory into
`/opt/baked-skills/` inside the
container image. The runtime cont-init step (`docker/hermes-cont-init.sh`)
seeds `/opt/data/skills/` from `/opt/baked-skills/` on each container
start using `cp -Rn` (no-clobber), so:

* A fresh `docker compose up --build` ships with the full `kb-*` catalog
  visible to `GET /v1/skills`.
* User edits applied directly under `./data/hermes/skills/` survive
  image rebuilds — the entrypoint never overwrites them.
* New skills baked into the image since the volume was last populated
  land on the next start.

## Editing a skill

1. Edit a `SKILL.md` (or `references/…` file) under `Sources/App/Resources/HermesSkills/<slug>/`.
2. `docker compose build hermes` to rebuild the image.
3. `docker compose up -d hermes` to restart the container.

For fast iteration, you can also edit `./data/hermes/skills/<slug>/`
directly — the volume bind-mount surfaces those edits live without a
rebuild. Just remember to fold the changes back into `Sources/App/Resources/HermesSkills/`
before committing so the next clean rebuild picks them up.

## Adding a new skill

1. Create `Sources/App/Resources/HermesSkills/<slug>/SKILL.md` with frontmatter:
   ```
   ---
   name: <slug>
   description: …
   trigger: /<slug>
   ---
   ```
2. Optional `references/`, `examples/`, `prompts/` subdirectories
   alongside `SKILL.md` work the same as in `~/.hermes/skills/`.
3. Commit + rebuild.

## Why bake instead of bind-mount?

A bind-mount of `~/.hermes/skills/` would tie production deploys to
the dev machine's host state. Baking the skills into the image is
reproducible across machines and CI — and removes the
`HERMES_HOME` overlay hazard that would otherwise shadow the runtime
state directory.
