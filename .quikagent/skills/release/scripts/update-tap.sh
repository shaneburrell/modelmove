#!/usr/bin/env bash
# Push Formula/modelmove.rb for a release to shaneburrell/homebrew-tap and
# smoke test it. Uses local gh/git credentials (no CI tap token).
#
# Usage: update-tap.sh [vX.Y.Z] [--dry-run] [--no-test]
set -euo pipefail

REPO="${MODELMOVE_REPO:-shaneburrell/modelmove}"
TAP="${MODELMOVE_TAP:-shaneburrell/homebrew-tap}"
# Homebrew addresses the repo "owner/homebrew-tap" as "owner/tap".
TAP_NAME="${TAP%/*}/${TAP#*/homebrew-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TAG=""
DRY_RUN=0
RUN_TEST=1
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --no-test) RUN_TEST=0 ;;
    -*) echo "error: unknown flag $arg" >&2; exit 1 ;;
    *) TAG="$arg" ;;
  esac
done

if [[ -z "$TAG" ]]; then
  TAG="$(gh release view --repo "$REPO" --json tagName -q .tagName)"
fi
VERSION="${TAG#v}"

FORMULA="$(mktemp)"
trap 'rm -f "$FORMULA"' EXIT
"$HERE/gen-formula.sh" "$TAG" > "$FORMULA"

if [[ "$DRY_RUN" -eq 1 ]]; then
  cat "$FORMULA"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -f "$FORMULA"; rm -rf "$WORK"' EXIT

if [[ -n "${TAP_DIR:-}" && -d "${TAP_DIR}/.git" ]]; then
  git -C "$TAP_DIR" fetch -q origin
  git -C "$TAP_DIR" checkout -q "$(git -C "$TAP_DIR" rev-parse --abbrev-ref origin/HEAD | sed 's#^origin/##')"
  git -C "$TAP_DIR" pull -q --ff-only
  TAP_WORK="$TAP_DIR"
else
  gh repo clone "$TAP" "$WORK/tap" -- --quiet
  TAP_WORK="$WORK/tap"
fi

cp "$FORMULA" "$TAP_WORK/Formula/modelmove.rb"

git -C "$TAP_WORK" add Formula/modelmove.rb
if git -C "$TAP_WORK" diff --cached --quiet; then
  echo "tap already at $TAG, nothing to push"
else
  git -C "$TAP_WORK" commit -q -m "modelmove $TAG"
  git -C "$TAP_WORK" push -q origin HEAD
  echo "pushed modelmove $TAG to $TAP"
fi

GIT_DIR="$(git -C "$HERE" rev-parse --git-dir 2>/dev/null || true)"
[[ -n "$GIT_DIR" ]] && rm -f "$GIT_DIR/modelmove-tap-pending"

if [[ "$RUN_TEST" -eq 0 ]]; then
  exit 0
fi

if command -v brew >/dev/null; then
  brew tap "$TAP_NAME" >/dev/null 2>&1 || true
  brew update >/dev/null 2>&1 || true
  brew reinstall --force "$TAP_NAME/modelmove" >/dev/null
  got="$(modelmove --version)"
  if [[ "$got" != *"$VERSION"* ]]; then
    echo "error: expected version $VERSION, got: $got" >&2
    exit 1
  fi
  brew test "$TAP_NAME/modelmove" >/dev/null
  echo "verified: $got"
  exit 0
fi

# Linux lab: no Homebrew. Smoke the published linux/amd64 tarball.
SMOKE="$WORK/smoke"
mkdir -p "$SMOKE"
if ! gh release download "$TAG" --repo "$REPO" -p "modelmove_${VERSION}_linux_amd64.tar.gz" -D "$SMOKE"; then
  echo "error: could not download linux/amd64 archive for $TAG" >&2
  exit 1
fi
tar -xzf "$SMOKE/modelmove_${VERSION}_linux_amd64.tar.gz" -C "$SMOKE"
got="$("$SMOKE/modelmove" --version)"
if [[ "$got" != *"$VERSION"* ]]; then
  echo "error: expected version $VERSION, got: $got" >&2
  exit 1
fi
echo "verified (linux tarball): $got"
