#!/usr/bin/env python3
"""Cases for harness.swift: curated ones, then seeded random documents.

    gen_cases.py            the checked-in set
    gen_cases.py --fuzz N   N random documents per op, for a local run
"""
import json
import random
import sys

TENANT = "11111111-2222-3333-4444-555555555555"
TENANT2 = "ABCDEF01-2345-6789-ABCD-EF0123456789"
MEMORY = "6f0a8a56-a701-4b28-8000-2f6579ebe065"

# Pieces chosen for where Swift's Character and Foundation semantics part from
# bytes and runes.
ATOMS = [
    "a", "b", "note", "word ", " ", "  ", "    ", "\t", "\n", "\n", "\n", "\n\n", "\r\n", "\r", "#", "# ", "## ", "### ", "####### ",
    "---", "===", "-", "=", "```", "~~~", "`", "[[", "]]", "[", "]", "|", "\\", "/", "./", ".", ".md", ".markdown", ".txt",
    "é", "é", "́", "̸", "≠", "#́", "[́", "]́", "|́", "`́", "\\́", "/́", "-́", " ́", ".́",
    "`", ";", "K", "​", "‌", "‍", " ", "　", " ", "\u0085", " ", " ", "﻿", "­", "᠎",
    "👨‍👩‍👧‍👦", "🇧🇷", "🇺🇸🇧", "👍🏽", "❤️", "©‍😀", "😀́‍😀", "😀️ः‍😀",
    "क्ष", "क्‍ष", "क़्ष", "न्‌न", "ক্ষ", "ത്‍ര", "กำ", "ກຳ", "မါ", "မး", "؀a", "࣢b", "각", "각", "각ᆨ", "각ᅡ", "한글", "日本語", "カタカナﾞ", "مرحبا", "Привет",
    "\U000e0067\U000e007f", "\U0001f3fb", "\U0001d173", "ힰ", "ퟋ", "ำ", "്", "্",
]
ATOMS += ["\u0600", "\u0903", "\u0897", "\\\u0903", "\\\u200d", "\\\u0e33", "\\\u200d\u0301", "\u0600\\", "\u0301\\", "\\\\\u0301", "\\\U0001f3fb"]
LINK_ATOMS = ["[[", "]]", "[[note]]", "[[a/b.md#H|L]]", "[[#x]]", "[[ a | b ]]", "[[x#]]", "[[x|]]", "[[./x/]]", "[[\\y\\z]]", "|", "#"]
HEAD_ATOMS = ["# Title\n", "## Sub ##\n", "### C#\n", "Title\n=====\n", "Sub\n-----\n", "# Foo\n---\n", "---\n", "\t# tab\n", "   # three\n", "    # four\n", "#nospace\n", "#\n", "####### seven\n", "```\n# in fence\n```\n", "~~~\n", "\n\n"]


def rand_doc(rng, atoms, n):
    return "".join(rng.choice(atoms) for _ in range(n))


def long_doc(rng, atom, lines):
    return "\n".join("".join(rng.choice(atom) for _ in range(rng.randint(1, 24))) for _ in range(lines))


def curated():
    out = []
    add = lambda op, **kw: out.append({"op": op, **kw})
    for s in ["", "abc", "é", "日本語", "é"]:
        add("sha", s=s)
    for t, s in [
        (TENANT, "projects/hermes.md"), (TENANT, "./projects/hermes.md"), (TENANT, "/projects/hermes.md"),
        (TENANT, "projects\\hermes.md"), (TENANT, "notas/café.md"), (TENANT, "notas/café.md"), (TENANT, "日本語/メモ.md"),
        (TENANT, "emoji/👨‍👩‍👧‍👦.md"), (TENANT, ".//./x"), (TENANT, "./́x"), (TENANT, "\\́x"), (TENANT2, "projects/hermes.md"),
        (TENANT2.lower(), "projects/hermes.md"), (TENANT, ""),
    ]:
        add("docid", t=t, s=s)
    add("docid-memory", t=TENANT, s=MEMORY)
    for d, o, h in [("lv_doc_0", 1, "aa"), ("lv_doc_0", 2, "aa"), ("lv_doc_0", 1, "bb"), ("lv_doc_é", 12, "cc")]:
        add("chunkid", d=d, o=o, h=h)
    for s in ["a\r\nb\nc\rd\n\r\ne", "\ńx\n", "\r\n", "\n", "", "a\n\r\n\nb", "\r\r\n\n"]:
        add("lines", s=s)
    for s in ["a\r\nb é 👨‍👩‍👧‍👦 🇧🇷🇺🇸 क्ष", "กำ", "မါ", "🇧🇷🇧", "😀‍😀", "a‍😀", "؀a"]:
        add("count", s=s)
    for s in ["​  \t x　 \u0085᠎   ﻿", " ́x", " x", "x‌", "　　"]:
        add("trim", s=s)
    for s in ["./Notes/A.md", "/Notes/A.md", "Notes\\A.md", "a/", "a//", "/", "./", "\\", "a/́", "\\́", "././x", ".//x", "//./x"]:
        add("norm", s=s)
        add("slug", s=s)
    for s in ["a/note.markdown", "a/note.md", "a/note", "note.txt", "/", "", "a/́b", "note.md́", ".md", "a/.md", "note.MD", "é.md"]:
        add("stem", s=s)
    add("resolve", links=["projects/hermes", "projects/hermes.md", "hermes", "index", "a/index", "missing", "hermes#H", "hermes|L", "café", "café", "a/café", "x.txt", "y"],
        cands=["projects/hermes.md", "a/index.md", "b/index.md", "notas/café.md", "x.txt", "y.markdown", "z/y.md"])
    add("resolve", links=["y"], cands=["y.markdown", "y.md"])
    add("resolve", links=["dup"], cands=["./dup.md", "dup.md"])
    for s in [
        "See [[hermes]] for details.", "[[projects/hermes#Fallbacks|how routing fails]]", "intro\n[[a]] and [[b]] and [[c]]\noutro",
        "[[real]]\n\n```\n[[not-a-link]]\n```\n\n[[alsoreal]]", "type `[[literal]]` to make [[real]]", "jump to [[#Section]]",
        "open [[never closed", "[[]]", "[[   ]]", "a [markdown](link) and [ref]", "[[" + "x" * 513 + "]]", "[[" + "x" * 512 + "]]",
        "[[" + "é" * 512 + "]]", "[[" + "é" * 513 + "]]", "`[[in-varia-code]]` [[out]]", "[[a [[b]] c]]", "[[a]]]", "[[a|b|c]]", "[[a#b#c]]",
        "[[á|b]]", "[[a|́b]]", "[́[x]]", "[[x]́]", "[[x]] [[y\r\n[[z]]", "```́\n[[not fenced]]\n", "```\n[[fenced]]\n```\n[[after]]",
        "[[ ./x/ ]]", "[[\\y\\z]]", "[[x#]]", "[[x|]]", "[[#|x]]", "~~~\n[[a]]\n```\n[[b]]\n~~~\n[[c]]",
    ]:
        add("links", s=s)
    docs = [
        "", "\n\n   \n\t\n", "# Hermes routing\n\nThe router picks a provider per request.\n\n## Fallbacks\n\nWhen the primary provider errors we retry once, then fall through to the\nsecondary. The escalation codeword is zarquonfallback.\n\n## Timeouts\n\nThirty seconds, then abort. The abort marker is zarquontimeout.",
        "# A\r\n\r\nbody\r\n## B\r\nmore", "---\ntitle: x\n---\n\n# R\n\nbody", "---\ntitle: broken\n\nstill content", "---\n---", "---", "  ---  \nk: v\n ---\nbody", "---\r\nt: x\r\n---\r\nbody",
        "Title\n=====\n\nbody\n\nSubtitle\n--------\n\nmore", "# Foo\n---\n\nbody", "---\n---\n---\nx\n---", "a\n===\n===\nb", "# C#\nx", "## Title ##\nx", "#\nx", "####### seven\nx", "    # four\nx", "\t# tab\nx",
        "# Real\n\n```bash\n# not a heading\necho hi\n```\n\ntext", "# Café ☕\n\nbody é", "# 日本語\n\n本文です。\n\n## 見出し\n\n内容", "# 👨‍👩‍👧‍👦 family\n\n🇧🇷🇧🇷", "#́ not heading\nbody", "--́\nx", "x\n--́",
    ]
    for s in docs:
        add("chunk", s=s)
        add("links", s=s)
        add("headings", s=s)
    rng = random.Random(4)
    # Each script's Characters and bytes disagree, so a cut measured the wrong way
    # lands on a different line. Line lengths straddle the limits on purpose.
    for atom in (["क्ष", "कि", "र्क", " "], ["กำ", "น้", "ที่", " "], ["မါ", "မး", "က", " "], ["é", "é", "ñ", " "], ["👨‍👩‍👧‍👦", "🇧🇷", "👍🏽", " "], ["한", "가", "글", " "], ["日", "本", "語", "。"]):
        for mx, ov in [(40, 10), (25, 24)]:
            add("chunk", s=long_doc(rng, atom, 14), max=mx, ov=ov)
            add("chunk", s=long_doc(rng, atom, 8).replace("\n", "\n\n", 5), max=mx, ov=ov)
    add("chunk", s="\n".join(f"line {i} of an unstructured capture" for i in range(1, 201)), max=300, ov=50)
    add("chunk", s="\n".join(f"sentence number {i}." for i in range(1, 61)), max=50, ov=49)
    add("chunk", s="\n\n".join(f"paragraph {i}" for i in range(1, 41)), max=120, ov=60)
    add("chunk", s="x" * 900, max=100, ov=10)
    add("chunk", s="\n".join("palavra " * 40 for _ in range(30)))
    return out


def fuzz(n, seed):
    rng = random.Random(seed)
    out = []
    for _ in range(n):
        size = rng.randint(1, 40)
        out.append({"op": "links", "s": rand_doc(rng, ATOMS + LINK_ATOMS * 4, size)})
        out.append({"op": "headings", "s": rand_doc(rng, ATOMS + HEAD_ATOMS * 4, size)})
        out.append({"op": "chunk", "s": rand_doc(rng, ATOMS + HEAD_ATOMS * 2, size * 3), "max": rng.choice([1, 5, 20, 60, 200]), "ov": rng.choice([0, 3, 10, 40, 300])})
        out.append({"op": "chunk", "s": rand_doc(rng, ATOMS + HEAD_ATOMS * 2, size)})
        out.append({"op": "count", "s": rand_doc(rng, ATOMS, size)})
        out.append({"op": "lines", "s": rand_doc(rng, ATOMS, size)})
        out.append({"op": "trim", "s": rand_doc(rng, ATOMS, 6)})
        for op in ("norm", "slug", "stem"):
            out.append({"op": op, "s": rand_doc(rng, ["/", "./", "\\", ".", "a", "é", ".md", ".txt", ".markdown", "́", "\r\n", "\u0903", "\u200d", "\u0600", " "], rng.randint(1, 8))})
        out.append({"op": "docid", "t": TENANT, "s": rand_doc(rng, ATOMS, 6)})
        links = [rand_doc(rng, ["a", "b", "é", "é", "/", ".md", "#h", "|l", "́", "\\"], rng.randint(1, 5)) for _ in range(4)]
        cands = [rand_doc(rng, ["a", "b", "é", "é", "/", ".md", ".txt", "́"], rng.randint(1, 5)) for _ in range(4)]
        out.append({"op": "resolve", "links": links, "cands": cands})
    return out


if len(sys.argv) > 2 and sys.argv[1] == "--fuzz":
    cases = fuzz(int(sys.argv[2]), 20261104)
else:
    cases = curated() + fuzz(12, 7)
json.dump(cases, sys.stdout, ensure_ascii=True)
