# agentlog

[![CI](https://github.com/liyixuan201211/agentlog/actions/workflows/ci.yml/badge.svg)](https://github.com/liyixuan201211/agentlog/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)](go.mod)

**Your AI coding agents already wrote a diary. Nothing could read it. Now something can.**

Every agent CLI keeps a private, undocumented transcript of everything you did together — and every one of them keeps it in a different format, in a different corner of your home directory. Ask yourself right now: *what did I tell Claude Code last Tuesday? What did that Codex session actually change? Where did all my tokens go this month?*

You can't answer, because the data is locked inside formats no tool reads.

`agentlog` reads them all. One binary, one command set, across every agent harness on your machine.

```console
$ agentlog sources
SOURCE   SESSIONS  NEWEST           STORE
claude       19  2026-09-11 17:16 claude
codex        31  2026-09-05 11:46 codex
dsh         480  2026-09-11 17:27 dsh

$ agentlog ls --limit 4
SOURCE  ID        MODIFIED             SIZE  TITLE
dsh     172eb54f- 2026-09-11 17:27   997.3K  做一个能刷星的有用GitHub项目
dsh     c194083a- 2026-09-11 17:27     2.0M  请为我建模一个学校的完整模
claude  8f8a747c- 2026-09-11 17:16     7.1M  ~/.dsh/profiles/sidebar-fix-handoff.md

$ agentlog search "rate limit" --limit 3
2026-09-11 17:19  dsh    66942462  assistant  AI 操作 UE5 的 MCP 或 Skill
    …the launcher itself is only getting 2.4MB/s. Hmm. Wait, maybe the proxy node hit a rate limit…
```

## Why this exists

There are already tools that read **Claude Code** history. They are good tools, and they share one limitation: the moment you also use Codex, or Cursor, or your own agent harness, they go blind. Your history is split across silos and nobody joins them up.

`agentlog` is built the other way around: a **harness-neutral transcript model** with one small adapter per store. Adding a new agent means adding one file. The commands never learn about a specific product.

## What you can do with it

| Command | What it answers |
|---|---|
| `agentlog ls` | What sessions do I have, across every harness? |
| `agentlog search "..."` | Have I solved this before? What did I say about it? |
| `agentlog show --last --full` | What actually happened in that session, including reasoning and tool output? |
| `agentlog export --last --format md` | Turn a session into a clean, shareable document |
| `agentlog stats --by model` | Where did the tokens go, per model or per harness? |

Search runs a raw-byte pre-filter before parsing, so it stays fast even with hundreds of sessions.

## Quick start

Requires Go 1.22 or newer.

```bash
git clone https://github.com/liyixuan201211/agentlog
cd agentlog
go build -o agentlog ./cmd/agentlog
```

Then point it at your history:

```bash
./agentlog sources                 # which stores were found
./agentlog ls --limit 20           # recent sessions
./agentlog search "migration"      # full-text search across all of them
./agentlog show --last --full      # read the newest one in detail

./agentlog export --last --format md > session.md
./agentlog export --last --format json | jq '.Usage'

./agentlog stats --by source
./agentlog stats --by model --limit 200
```

Add `--json` to `ls`, `search`, `show` and `stats` to get machine-readable output for scripting.

### Command reference

```
agentlog ls     [-source a,b] [-limit N] [-json] [-no-title]
agentlog search <query> [-source a,b] [-limit N] [-context N] [-turns] [-json]
agentlog show   [id|--last] [-full] [-no-tools] [-json]
agentlog export [id|--last] [-format md|json] [-full] [-out FILE]
agentlog stats  [-by source|model] [-limit N] [-json]
agentlog sources [-json]
```

Flags may be written before or after the query: `agentlog search "x" --limit 5` and `agentlog search --limit 5 "x"` both work.

## Output

`show` gives you a readable transcript; `--full` adds reasoning and tool output:

```
dsh  session-172eb54f-3a6c-4f75-bfb0-f53e2e5632b1
title  做一个能刷星的有用GitHub项目
cwd    /Users/imac
when   2026-09-11 16:37 → 2026-09-11 17:14  (36m54s)
model  deepseek-flash
333 messages, 183 tool calls
────────────────────────────────────────────────────────────────────────

USER ····
你随便为我做一个你认为极为有用的 GitHub 项目去刷 star

ASSISTANT ····
→ bash
→ web_search
```

`export --format md` produces a document with a metadata table, one section per turn, and tool output folded into `<details>` blocks so a long session stays readable.

## Supported stores

| Harness | Location | Status |
|---|---|---|
| Claude Code | `~/.claude/projects/*/*.jsonl` | full |
| Codex | `~/.codex/sessions/**/rollout-*.jsonl` | full |
| DeepSeek Harness (DSH) | `~/.dsh/sessions/*/*/session*.jsonl.zstd` | full |

Adapters are discovered at startup; a harness whose store is absent is simply not listed. Adding another is one file implementing two methods — see `internal/source/source.go`.

## Design notes

**A neutral model, not a lowest common denominator.** Every harness is normalised into `Transcript → Turn → Block`. Streaming chunk records are reassembled into final messages, reasoning is kept separate from prose, and tool calls are paired with their results. Usage accounting respects each harness's semantics — Claude reports per-turn tokens, Codex reports a running total, so summing them naively would be wrong. It isn't.

**Read-only, and honest about gaps.** `agentlog` never writes to your session stores. When a record cannot be parsed it is counted in `Transcript.Malformed` and reported, rather than silently dropped.

**A real zstd decoder, written from the spec.** DSH compresses its sessions with zstd, and Go's standard library has no zstd support. Rather than pull in a dependency, `internal/zstd` implements the frame format directly from RFC 8878 and the reference decoder's semantics: frame and block headers, raw/RLE/Huffman literals, FSE normalized-count tables, interleaved sequence decoding, and repeat-offset handling.

**One documented fallback.** FSE-compressed Huffman *weight* headers are parsed, but not yet decoded correctly by the pure-Go path. Instead of returning wrong bytes, `agentlog` reports that construct as unsupported and delegates to an installed `zstd` binary. Correct and slower beats fast and wrong. If no `zstd` is present, that path errors clearly instead of guessing.

```bash
./agentlog ls --json                    # uses the fallback where needed, invisible to you
```

Set `--backend=go` on `cmd/zstdprobe` to exercise the pure-Go decoder alone.

**Verification.** The decoder was validated against Python's `zstandard` on **487 real session files** (roughly 480 MB compressed), comparing a SHA-256 of every decoded byte: 487/487 identical. The test suite covers FSE table construction against the reference formula, the normalized-count reader against a captured payload, Huffman table construction from the format specification's own example, and all three adapters.

```bash
go test ./...
```

## Limits

- Read-only by design: `agentlog` cannot resume a session or send anything to an agent.
- The pre-filter is byte-level, so search is case-insensitive substring matching rather than full-text ranking.
- Token counts reflect what each harness recorded. A harness that does not log usage shows zero rather than an estimate.
- Claude's project directory slug is lossy, so the displayed working directory is an approximation; the transcript's own `cwd` field is used when present.
- Sessions currently being written can be read while they grow; a snapshot may differ from a later read of the same file.

## License

MIT
