#!/usr/bin/env python3
"""Generate modules.yaml from docs/migration/routes.json (Go migration Task 0.2).

Each module gets the smallest set of path prefixes that covers all its routes
and matches no other module's route, so one `ingress.extraPaths` entry per
prefix moves exactly that module. When no such prefix exists:
  - a parameter-free route goes under `exactPaths` (an Ingress pathType: Exact entry);
  - otherwise its longest literal prefix goes under `sharedPrefixes`, and the
    modules with routes under it are listed in `flipsWith`: those modules must
    reach production in the same infra PR.

A module may leave routes on Swift (`HELD` below): its prefix still covers them,
and an explicit ingress entry keeps them on Swift. They need no flip partner. A
held route of the module's own that no shared prefix covers is also listed under
`exactPaths`, the entry it takes when released; until then the hold wins.

The chart's `pathType: Prefix` on Traefik matches plain string prefixes (no
`strictPrefixMatching`), so `/v1/vault` also catches `/v1/vaults`. Prefixes are
still chosen by path element, and any route of another module caught only as a
string goes under `stringPrefixCatches`: a flip of that prefix must name those
paths explicitly in ingress, which is correct under either behaviour.

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
    "Devices": 2, "Settings": 6, "Weather": 2, "News": 2, "Kanban": 2, "Achievements": 2,
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
    # Pseudo-modules split out of a Swift dir by controller (SPLITS below).
    "MeGateways": 6, "MeProviders": 5,
    "MemoryAgent": 4, "KnowledgeReason": 4, "KBCompile": 4,
    # wave-5-plan.md, P6. `llm` is four surfaces with different auth and limits.
    # The cron bridge runs `docker exec`, which is wave 6 machinery. Wave 4 committed
    # to flipping /v1/ws at the end of wave 5, ahead of the rest of App.
    "LLMRouter": 5, "LLMVoice": 5, "LLMChat": 5, "LLMAdmin": 5, "WS": 5,
    "SkillsCron": 6,
    "Soul": 8,  # 8D: /v1/soul splits out of auth (18788ca)
}

# Some Swift dirs hold controllers that belong to a later wave
# (decisions.md D-me-split). A route whose source file is listed here is
# assigned to the named pseudo-module instead of its dir.
SPLITS = {
    "Sources/App/Me/HermesGatewaysController.swift": "MeGateways",
    "Sources/App/Me/ProvidersController.swift": "MeProviders",
    "Sources/App/Me/LLMPreferencesController.swift": "MeProviders",
    "Sources/App/Me/ConnectionsController.swift": "MeProviders",
    # Call the LLM transport (wave-4-plan.md, group 4F). GET /v1/memos travels
    # with POST: ingress can't split a path by method.
    "Sources/App/Memory/MemoController.swift": "MemoryAgent",
    "Sources/App/Memory/QueryController.swift": "MemoryAgent",
    "Sources/App/LLM/Routing/RouterController.swift": "LLMRouter",
    "Sources/App/LLM/Routing/ParallelController.swift": "LLMRouter",
    "Sources/App/LLM/OpenAIAudio/OpenAIAudioController.swift": "LLMVoice",
    "Sources/App/LLM/Transcribe/TranscribeController.swift": "LLMVoice",
    "Sources/App/LLM/LLMController.swift": "LLMChat",
    "Sources/App/LLM/Routing/LLMHealthController.swift": "LLMAdmin",
    "Sources/App/Skills/CronBridgeController.swift": "SkillsCron",
    "Sources/App/Auth/SoulController.swift": "Soul",
}
# Routes whose controller also holds routes of the parent module, or that are
# registered in App+build.swift.
SPLIT_PATHS = {
    "/v1/gateways/photon/inbound": "MeGateways",
    "/v1/memory/search": "MemoryAgent",
    "/v1/knowledge/reason": "KnowledgeReason",
    "/v1/knowledge/reason/stream": "KnowledgeReason",
    "/v1/memory-compile": "KBCompile",
    "/v1/ws": "WS",
}
# The smallest free prefix can be wider than the module means to take. /admin is
# free today only because the inventory has one route under it; ingress takes the
# route itself (wave-5-plan.md, "Ingress holds and traps").
MIN_PREFIX_ELEMENTS = {"LLMAdmin": 3}
# Routes a module leaves on Swift when it flips: path, reason, and the group that
# releases them. A held route is out of its module's G1 and in the releasing
# group's. "batch" means wave-5-plan.md, "Flip order and what flips together". The
# owner decision on /v1/admin/community is still open (flip-blockers.md); batch
# 5-3b adds or releases that hold accordingly.
HELD = {
    "Memory": [(
        "/v1/memory/search",
        "runs the agent loop (HermesMemoryService.search), which needs internal/llm; owned by memory-agent",
        "wave-4-agent",
    )],
    "KBCompile": [(
        "/v1/memory-compile",
        "must stay on the same server as /v1/ws: the iOS review sheet fills only from its .memorySaved frames",
        "batch 5-4",
    )],
    "Apple": [(
        "/v1/devices/command",
        "DeviceCommandBroker's continuations live in the server's memory: a result reaching another server resumes nothing",
        "batch 5-4",
    )],
    "Reminders": [(
        "/v1/reminders/detect",
        "ReminderIntentClassifier needs the LLM; Go's UnavailableTransport would answer isReminder false",
        "batch 5-3b",
    )],
    "Me": [(
        "/v1/me/recap/period",
        "the claim and the themes worker share a process: on Go the row is claimed and nothing generates it",
        "batch 5-3b",
    )],
    "Community": [(
        "/v1/admin/community",
        "the daily run needs the LLM: on Go it can only record failed attempts, and three cost that day's post",
        "batch 5-3b",
    )],
}
# Must flip together though no prefix is shared: Gmail connect finishes at
# /v1/calendar/oauth/callback, which only finds OAuth sessions from its own process.
COUPLED = {"Calendar": {"Mail"}}
SWIFT_DIRS = {
    "Soul": "Sources/App/Auth",
    "App": "Sources/App/App+build.swift",
    "MeGateways": "Sources/App/Me",
    "MeProviders": "Sources/App/Me",
    "MemoryAgent": "Sources/App/Memory",
    "KnowledgeReason": "Sources/App/Knowledge",
    "KBCompile": "Sources/App/KB",
    "LLMRouter": "Sources/App/LLM/Routing",
    "LLMVoice": "Sources/App/LLM",
    "LLMChat": "Sources/App/LLM",
    "LLMAdmin": "Sources/App/LLM/Routing",
    "SkillsCron": "Sources/App/Skills",
    "WS": "Sources/App/App+build.swift",
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


def string_only(prefix: str, path: str) -> bool:
    """The route is caught by the prefix as a string, but not by path element."""
    return path.startswith(prefix) and not matches(segments(prefix), segments(path))


def under(prefix: list[str], path: list[str]) -> bool:
    """Literal containment, as in internal/manifest: a {param} is not a wildcard here."""
    return path[: len(prefix)] == prefix


def main() -> int:
    routes = json.loads(ROUTES.read_text())
    by_module: dict[str, list[list[str]]] = {}
    held_segments = {m: [segments(path) for path, _, _ in held] for m, held in HELD.items()}
    for r in routes:
        module = SPLIT_PATHS.get(r["path"]) or SPLITS.get(r["source"].split(":")[0], r["module"])
        by_module.setdefault(module, []).append(segments(r["path"]))

    missing_wave = sorted(m for m in by_module if m not in WAVES)
    if missing_wave:
        print("modules without a wave in WAVES:", missing_wave)
        return 1

    plan = {}
    for module, paths in sorted(by_module.items()):
        # A route this module holds doesn't stop it taking the shorter prefix.
        holds = held_segments.get(module, [])
        others = [p for m, ps in by_module.items() if m != module for p in ps if p not in holds]
        prefixes, exact, shared, partners = set(), set(), set(), set()
        for path in paths:
            chosen = None
            for n in range(MIN_PREFIX_ELEMENTS.get(module, 1), len(path) + 1):
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
                    partners |= {m for m, ps in by_module.items() if m != module and any(matches(literal, q) for q in ps if q not in holds)}
            else:
                prefixes.add("/" + "/".join(chosen))
        # Drop prefixes covered by a shorter one of the same module.
        prefixes = {p for p in prefixes if not any(q != p and p.startswith(q + "/") for q in prefixes)}
        prefixes -= shared
        # A held route of its own stops counting as claimed by the prefix.
        for hold in holds:
            for path in paths:
                route = "/" + "/".join(path)
                literal = not any(s.startswith("{") or s.startswith("*") for s in path)
                if literal and under(hold, path) and not any(matches(segments(s), path) for s in shared):
                    exact.add(route)
        plan[module] = (sorted(prefixes), sorted(exact), sorted(shared), sorted(partners))

    for module, others in COUPLED.items():
        plan[module][3].extend(others)
    # Flip partnership is mutual: if A's shared prefix covers B's routes, B
    # must flip with A as well.
    for module, (_, _, _, partners) in list(plan.items()):
        for other in partners:
            if module not in plan[other][3]:
                plan[other][3].append(module)
    plan = {m: (p, e, s, sorted(set(ps))) for m, (p, e, s, ps) in plan.items()}

    # What each module's prefixes catch of other modules' routes, only as strings.
    # Named by the other module's own entries, which is what ingress has to list.
    catches: dict[str, set[str]] = {m: set() for m in plan}
    for module, (prefixes, _, shared, _) in plan.items():
        for prefix in prefixes + shared:
            for other, (o_prefixes, o_exact, o_shared, _) in plan.items():
                if other == module:
                    continue
                for path in by_module[other]:
                    route = "/" + "/".join(path)
                    if not string_only(prefix, route):
                        continue
                    entries = [e for e in o_exact if e == route]
                    entries += [e for e in o_prefixes + o_shared if matches(segments(e), path)]
                    catches[module].update(entries or [route])

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
            f"    swiftDir: {SWIFT_DIRS.get(module, f'Sources/App/{module}')}",
            f"    status: {existing.get(key, 'swift')}",
            f"    prefixes: [{', '.join(prefixes)}]",
        ]
        if exact:
            lines.append(f"    exactPaths: [{', '.join(exact)}]")
        if shared:
            lines.append(f"    sharedPrefixes: [{', '.join(shared)}]")
        if catches[module]:
            lines.append(f"    stringPrefixCatches: [{', '.join(sorted(catches[module]))}]")
        if module in HELD:
            lines.append("    heldPaths:")
            for path, reason, released_by in HELD[module]:
                lines.append(f"      - {{path: {path}, reason: {json.dumps(reason)}, releasedBy: {json.dumps(released_by)}}}")
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
