#!/command/with-contenv sh
# s6-overlay cont-init step for the LuminaVault Hermes image.
#
# Runs as root at container start, AFTER the base image's
# /etc/cont-init.d/01-hermes-setup (UID remap, volume chown, config
# seeding) and BEFORE the gateway starts. The base image supervises
# everything under s6-overlay (/init is PID 1) and drops each service to
# the `hermes` user with s6-setuidgid, so this is a cont-init script rather
# than an entrypoint wrapper — the previous tini + gosu wrapper assumed a
# base image that no longer exists.
#
# Because the base hook has already chowned the volume by the time this
# runs, anything created here is chowned to `hermes` explicitly. The old
# entrypoint relied on the base chowning after it; that ordering is gone.
set -eu

HERMES_HOME="${HERMES_HOME:-/opt/data}"
BAKED="/opt/baked-skills"
TARGET="${HERMES_HOME}/skills"

# HER-276 — seed baked kb-* skills. `cp -Rn` is no-clobber: skill files
# already on the volume (edited, or persisted runtime state) are kept, and
# skills baked into the image since the volume was last seeded land now.
if [ -d "${BAKED}" ]; then
    mkdir -p "${TARGET}"
    cp -Rn "${BAKED}/." "${TARGET}/" 2>/dev/null || true
    chown -R hermes:hermes "${TARGET}" 2>/dev/null || true
fi

# Mnemosyne's store must exist on the volume before Hermes spawns
# `mnemosyne mcp`, and be writable by the runtime user.
mkdir -p "${HERMES_HOME}/mnemosyne"
chown -R hermes:hermes "${HERMES_HOME}/mnemosyne" 2>/dev/null || true

# HER-85/100 — Hummingbird mirrors SOUL.md into profiles/<username>/ on
# PUT /v1/soul, as a different uid from Hermes. Traverse-only on the data
# root; shared write on profiles/ with sticky-tmpdir semantics so either
# service can create per-user dirs safely.
mkdir -p "${HERMES_HOME}/profiles"
chmod 711 "${HERMES_HOME}" 2>/dev/null || true
chmod 1777 "${HERMES_HOME}/profiles" 2>/dev/null || true

exit 0
