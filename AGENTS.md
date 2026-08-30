# AGENTS.md — modelmove

Tool-agnostic guidance for coding agents working in this repo
(quikagent, Cursor, Claude Code, etc.).

## Overview

**modelmove** is a Go CLI for sparse-delta, verified transfer of LLM
weight directories (Hugging Face, GGUF, Ollama, generic). Local and SSH
transports are the supported path. Module: `github.com/shaneburrell/modelmove`.
Entry: `cmd/modelmove`. License: MIT.

Stay on **synthetic fixtures** (a few MiB) in this lab. Do not download
multi-gigabyte checkpoints onto the 50 GiB lab disk.

## Build and test

```sh
gofmt -s -w .
go vet ./...
golangci-lint run ./...
go test ./...
go test -race ./...
make cover          # fails below 80%
make build
./scripts/e2e.sh    # fake-SSH + local copy/sync/verify
```

`make check` runs fmt-check, vet, lint, and test (not race, cover, or e2e).

After any change that touches copy/sync/verify/SSH, also run the live
sshd smoke (skips when `ubuntu@127.0.0.1` is unreachable):

```sh
make build
./scripts/e2e.sh
./scripts/e2e-live.sh
```

## Layout

| Path | Role |
|------|------|
| `cmd/modelmove` | CLI entry |
| `internal/cli` | Cobra commands |
| `internal/engine` | scan → plan → stream → verify |
| `internal/chunk` | FastCDC + BLAKE3 |
| `internal/layout` | HF / GGUF / Ollama / generic |
| `internal/manifest` | content-addressed manifests |
| `internal/scan` | directory walk and hashing |
| `internal/receiver` | staging under `.modelmove/` |
| `internal/transport` | local + SSH |
| `internal/protocol` | framed local ↔ remote-helper |
| `scripts/e2e.sh` | synthetic local + fake-SSH smoke |

## Conventions

- Match existing Go style: small packages, table-driven tests.
- Prefer editing existing files over new abstraction layers.
- Do not add heavy frameworks.
- File tools stay inside the workdir; `bash` is not filesystem-sandboxed.

## Git and pull requests

- Feature branch off `main`. Never commit on `main` unless asked.
- Never commit API keys, tokens, or `~/.quikagent/config.yaml`.
- Commit subject: one sentence, why not what.
- Open a PR with `gh pr create`. Wait for GitHub CI (fmt, tidy, vet,
  test, race, cover, lint, fuzz, matrix build, e2e).
- Squash-merge only when asked. Default: wait for the operator.

## Releases (operator approves each tag)

Current scheme: `v0.1.x`. Latest published before the lab cycles: `v0.1.3`.
Lab cycles ship **v0.1.4–v0.1.8**.

Only after the intended commits are on `main` and CI is green. Never
force-push tags. **Do not tag until the operator names the version or
says to tag.** Then:

1. Load the `release` skill (`skill` tool, name `release`).
2. Follow it: annotated tag, push, watch GoReleaser, bump the Homebrew
   tap, smoke-test the linux/amd64 binary.

```sh
git fetch --tags && git tag -l 'v*' --sort=-v:refname | head
gh release list --limit 5
git checkout main && git pull --ff-only origin main
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
gh run watch --exit-status $(gh run list --workflow=release.yml --limit 1 --json databaseId -q '.[0].databaseId')
.quikagent/skills/release/scripts/update-tap.sh vX.Y.Z
```

Pushing `v*` runs `.github/workflows/release.yml` (GoReleaser). The tap
lives in a **different** repo (`shaneburrell/homebrew-tap`); CI has no
token for it. Update it locally with the release skill scripts.

This lab VM has no Homebrew. The tap script still pushes `Formula/modelmove.rb`
and smokes the downloaded linux/amd64 tarball (`modelmove --version`
must contain the tag version).

## Secrets and safety

- Never commit or print API keys, `gh` tokens, or SSH private keys.
- Do not bind services on the LAN.
- Do not `brew`/`apt` install large model weights.

## Out of scope for lab cycles unless the gap list says otherwise

- QUIC transport
- Full Hugging Face Hub or Ollama registry clients
- A second GPU box or real 70 GB checkpoints
