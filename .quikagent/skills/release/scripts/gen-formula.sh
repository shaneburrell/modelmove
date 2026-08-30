#!/usr/bin/env bash
# Print Formula/modelmove.rb for a shaneburrell/modelmove GitHub Release tag.
# Usage: gen-formula.sh [vX.Y.Z]
set -euo pipefail

REPO="${MODELMOVE_REPO:-shaneburrell/modelmove}"
TAG="${1:-}"

if [[ -z "$TAG" ]]; then
  TAG="$(gh release view --repo "$REPO" --json tagName -q .tagName)"
fi

if [[ ! "$TAG" =~ ^v[0-9] ]]; then
  echo "error: tag must look like v0.2.0 (got $TAG)" >&2
  exit 1
fi

VERSION="${TAG#v}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# GoReleaser uploads checksums.txt last, so a tag push and this script can race.
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  if gh release download "$TAG" --repo "$REPO" -p checksums.txt -D "$TMP" 2>/dev/null; then
    break
  fi
  if [[ "$attempt" -eq 10 ]]; then
    echo "error: checksums.txt never appeared on $TAG; is the release workflow still running?" >&2
    exit 1
  fi
  echo "waiting for checksums.txt on $TAG (attempt $attempt)..." >&2
  sleep 15
done

CHECKSUMS="$TMP/checksums.txt"

sha_for() {
  local asset="$1" line
  line="$(grep -E "[[:space:]]${asset}\$" "$CHECKSUMS" || true)"
  if [[ -z "$line" ]]; then
    echo "error: missing checksum for $asset" >&2
    exit 1
  fi
  awk '{print $1}' <<<"$line"
}

# Homebrew only needs macOS and Linux; the freebsd and windows artifacts are
# published but have no formula equivalent.
DARWIN_AMD64="modelmove_${VERSION}_darwin_amd64.tar.gz"
DARWIN_ARM64="modelmove_${VERSION}_darwin_arm64.tar.gz"
LINUX_AMD64="modelmove_${VERSION}_linux_amd64.tar.gz"
LINUX_ARM64="modelmove_${VERSION}_linux_arm64.tar.gz"

SHA_DARWIN_AMD64="$(sha_for "$DARWIN_AMD64")"
SHA_DARWIN_ARM64="$(sha_for "$DARWIN_ARM64")"
SHA_LINUX_AMD64="$(sha_for "$LINUX_AMD64")"
SHA_LINUX_ARM64="$(sha_for "$LINUX_ARM64")"

BASE="https://github.com/${REPO}/releases/download/${TAG}"

cat <<EOF
class Modelmove < Formula
  desc "Sparse-delta, verified transfer and migration of LLM model weights"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "MIT"

  on_macos do
    on_arm do
      url "${BASE}/${DARWIN_ARM64}"
      sha256 "${SHA_DARWIN_ARM64}"
    end
    on_intel do
      url "${BASE}/${DARWIN_AMD64}"
      sha256 "${SHA_DARWIN_AMD64}"
    end
  end

  on_linux do
    on_arm do
      url "${BASE}/${LINUX_ARM64}"
      sha256 "${SHA_LINUX_ARM64}"
    end
    on_intel do
      url "${BASE}/${LINUX_AMD64}"
      sha256 "${SHA_LINUX_AMD64}"
    end
  end

  def install
    bin.install "modelmove"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/modelmove --version")
  end
end
EOF
