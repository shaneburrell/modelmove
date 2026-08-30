---
name: release
description: >-
  Cut a modelmove v* GitHub release (annotated tag, GoReleaser) and bump
  shaneburrell/homebrew-tap Formula/modelmove.rb. Use when the operator
  approves a tag, after CI is green on main, or when asked to update the
  Homebrew tap.
---

# Release modelmove

Only after the intended commits are on `main` and CI is green. Never
force-push tags. Do not tag until the operator names the version or
says to tag.

## Version

1. `git fetch --tags && git tag -l 'v*' --sort=-v:refname | head`
2. `gh release list --limit 5`
3. Next patch unless the operator names a version. Scheme: `v0.1.x`.

## Tag

```sh
git checkout main && git pull --ff-only origin main
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

Pushing `v*` runs `.github/workflows/release.yml` (GoReleaser).

## Watch

```sh
gh run list --workflow=release.yml --limit 3
gh run watch <id> --exit-status
gh release view vX.Y.Z
```

Wait until `checksums.txt` exists on the release.

## Homebrew tap

The tap is a different repository. CI has no token for it. Run:

```sh
.quikagent/skills/release/scripts/update-tap.sh vX.Y.Z
```

The script regenerates `Formula/modelmove.rb` from `checksums.txt`,
pushes to `shaneburrell/homebrew-tap` if it changed, then smokes:

- **macOS / brew present:** `brew reinstall` and `modelmove --version`
- **Linux lab (no brew):** download `modelmove_${VERSION}_linux_amd64.tar.gz`
  and check `modelmove --version` contains the version

Flags: `--dry-run` (print formula only), `--no-test` (skip smoke).

Do not add a GoReleaser `brews:` block or a tap token to repo secrets.

## Report

State the GitHub release URL, the tap commit (or "already at tag"), and
the version string the smoke test printed. If auth or network fails,
say so plainly — a release with a stale tap means `brew install` serves
the old binary.
