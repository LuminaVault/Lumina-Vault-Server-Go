#!/usr/bin/env python3
"""Generate modules.yaml from docs/migration/routes.json (Go migration Task 0.2).

Each module gets the smallest set of path prefixes that covers all its routes
and matches no other module's route, so one `ingress.extraPaths` entry per
prefix moves exactly that module. When no such prefix exists:
  - a parameter-free route goes under `exactPaths` (an Ingress pathType: Exact entry);
  - otherwise its longest literal prefix goes under `sharedPrefixes`, and the
    modules with routes under it are listed in `flipsWith`: those modules must
    reach production in the same infra PR.

Statuses of modules already in modules.yaml are kept, so re-running after the
route inventory changes never resets progress.

    python3 scripts/gen-modules.py            # write modules.yaml
    python3 scripts/gen-modules.py --check    # exit 1 if modules.yaml is stale
"""
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ROUTES = ROOT / "docs/migration/routes.json"
OUT = ROOT / "modules.yaml"

# Wave per Swift module dir: the roadmap's module table. Unlisted → 0, which
# means "assign a wave" (gen fails until every module has one).
WAVES = {
    # Pilot (decisions.md D-pilot): Projects is pure CRUD. Todos writes vault
    # files, memories and embeddings, so it moves with the vault in wave 4.
    "Projects": 1,
    "Reminders": 2, "Apple": 2, "Profiles": 2, "Sessions": 2, "Spaces": 2,
    "Devices": 2, "Settings": 2, "Weather": 2, "News": 2, "Kanban": 2, "Achievements": 2,
    "Dashboard": 3, "Health": 3, "Calendar": 3, "Mail": 3, "Community": 3, "Marketplace": 3,
    "Account": 3, "Me": 3, "Onboarding": 3, "Teams": 3,
    "Todos": 4, "Vault": 4, "Memory": 4, "Knowledge": 4, "KB": 4, "Capture": 4, "Import": 4,
    "Ingestion": 4, "Indexing": 4, "Vision": 4, "Repositories": 4,
    "LLM": 5, "Chat": 5, "Transcribe": 5, "TTS": 5, "SelfImprovement": 5, "Skills": 5,
    "Agents": 5, "MCP": 5, "Workflows": 5, "Grok": 5,
    "Hermes": 6, "HermesGateways": 6, "Plugins": 6, "Integrations": 6,
    "Billing": 7, "Admin": 7, "System": 7, "Telemetry": 7, "WebSockets": 7,
    "Auth": 8, "Crypto": 8,
    "App": 8,  # root-mounted routes (/health, AASA, /v1/ws, /v1/auth/me*) move with the final cutover
}


def kebab(name: str) -> str:
    return re.sub(r"(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])", "-", name).lower()


def segments(path: str) -> list[str]:
    return [s for s in path.split("/") if s]


def matches(prefix: list[str], path: list[str]) -> bool:
    """Ingress Prefix semantics: element-wise; a {param} in the route matches anything."""
    if len(prefix) > len(path):
        return False
    return all(p == s or s.startswith("{") or s.startswith("*") for p, s in zip(prefix, path))


def main() -> int:
    routes = json.loads(ROUTES.read_text())
    by_module: dict[str, list[list[str]]] = {}
    for r in routes:
        by_module.setdefault(r["module"], []).append(segments(r["path"]))

    missing_wave = sorted(m for m in by_module if m not in WAVES)
    if missing_wave:
        print("modules without a wave in WAVES:", missing_wave)
        return 1

    plan = {}
    for module, paths in sorted(by_module.items()):
        others = [p for m, ps in by_module.items() if m != module for p in ps]
        prefixes, exact, shared, partners = set(), set(), set(), set()
        for path in paths:
            chosen = None
            for n in range(1, len(path) + 1):
                cand = path[:n]
                if any(s.startswith("{") or s.startswith("*") for s in cand):
                    break  # a literal prefix cannot contain a parameter
                if not any(matches(cand, o) for o in others):
                    chosen = cand
                    break
            if chosen is None:
                literal = []
                for seg in path:
                    if seg.startswith("{") or seg.startswith("*"):
                        break
                    literal.append(seg)
                if len(literal) == len(path):
                    exact.add("/" + "/".join(path))
                else:
                    shared.add("/" + "/".join(literal))
                    partners |= {m for m, ps in by_module.items() if m != module and any(matches(literal, q) for q in ps)}
            else:
                prefixes.add("/" + "/".join(chosen))
        # Drop prefixes covered by a shorter one of the same module.
        prefixes = {p for p in prefixes if not any(q != p and p.startswith(q + "/") for q in prefixes)}
        prefixes -= shared
        plan[module] = (sorted(prefixes), sorted(exact), sorted(shared), sorted(partners))

    # Flip partnership is mutual: if A's shared prefix covers B's routes, B
    # must flip with A as well.
    for module, (_, _, _, partners) in list(plan.items()):
        for other in partners:
            if module not in plan[other][3]:
                plan[other][3].append(module)
    plan = {m: (p, e, s, sorted(set(ps))) for m, (p, e, s, ps) in plan.items()}

    existing = {}
    if OUT.exists():
        current = None
        for line in OUT.read_text().splitlines():
            if m := re.match(r"^  ([a-z0-9-]+):$", line):
                current = m.group(1)
            elif (m := re.match(r"^    status:\s*(\S+)", line)) and current:
                existing[current] = m.group(1)

    lines = [
        "# Generated by scripts/gen-modules.py from docs/migration/routes.json.",
        "# Edit only `status` (and `jobs`); re-run the script after the route inventory changes.",
        "# status: swift | parity | staging | production | done  (lifecycle: see the roadmap)",
        "modules:",
    ]
    for module, (prefixes, exact, shared, partners) in sorted(plan.items(), key=lambda kv: (WAVES[kv[0]], kv[0])):
        key = kebab(module)
        lines += [
            f"  {key}:",
            f"    wave: {WAVES[module]}",
            f"    swiftDir: Sources/App/{module}" if module != "App" else "    swiftDir: Sources/App/App+build.swift",
            f"    status: {existing.get(key, 'swift')}",
            f"    prefixes: [{', '.join(prefixes)}]",
        ]
        if exact:
            lines.append(f"    exactPaths: [{', '.join(exact)}]")
        if shared:
            lines.append(f"    sharedPrefixes: [{', '.join(shared)}]")
        if partners:
            lines.append(f"    flipsWith: [{', '.join(kebab(m) for m in partners)}]")
            if any(WAVES[m] != WAVES[module] for m in partners):
                print(f"warning: {module} shares a prefix with a module in another wave: {partners}")
        lines.append("    jobs: []")
    text = "\n".join(lines) + "\n"

    if "--check" in sys.argv:
        if not OUT.exists() or OUT.read_text() != text:
            print("modules.yaml is stale; run scripts/gen-modules.py")
            return 1
        return 0
    OUT.write_text(text)
    print(f"{len(plan)} modules, {sum(len(v[0]) for v in plan.values())} prefixes, "
          f"{sum(len(v[1]) for v in plan.values())} exact paths, "
          f"{sum(1 for v in plan.values() if v[2])} modules with shared prefixes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
