#!/usr/bin/env bash
# Live-sshd smoke: same synthetic HF tree as e2e.sh, but the destination is
# ubuntu@127.0.0.1 over a real sshd. Skips cleanly when localhost SSH is
# unavailable so GitHub Actions and laptops without sshd stay green.
set -euo pipefail

BIN=${BIN:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/modelmove"}
if [ ! -x "$BIN" ]; then
  echo "e2e-live: $BIN not found; run 'make build' first" >&2
  exit 1
fi

REMOTE_USER=${MODELMOVE_LIVE_SSH_USER:-ubuntu}
REMOTE_HOST=${MODELMOVE_LIVE_SSH_HOST:-127.0.0.1}
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=2 -o StrictHostKeyChecking=accept-new)

if ! "${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" true >/dev/null 2>&1; then
  echo "e2e-live: skip (no live ssh to ${REMOTE_USER}@${REMOTE_HOST})"
  exit 0
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; "${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "rm -rf /tmp/modelmove-live-$$" >/dev/null 2>&1 || true' EXIT

REMOTE_DST="/tmp/modelmove-live-$$"
SRC="$WORK/src"
mkdir -p "$SRC"

echo "==> e2e-live: synthetic Hugging Face checkpoint"
python3 - "$SRC" <<'PY'
import os, random, sys
root = sys.argv[1]
random.seed(1234)
with open(os.path.join(root, "config.json"), "w") as f:
    f.write('{"model_type":"llama","hidden_size":4096}')
with open(os.path.join(root, "tokenizer.json"), "w") as f:
    f.write('{"vocab":[]}' + "x" * 20000)
with open(os.path.join(root, "model.safetensors.index.json"), "w") as f:
    f.write('{"weight_map":{}}')
for i in (1, 2):
    name = f"model-0000{i}-of-00002.safetensors"
    with open(os.path.join(root, name), "wb") as f:
        f.write(random.randbytes(4_000_000))
PY

fail() { echo "e2e-live: $1" >&2; exit 1; }

TARGET="${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DST}"

echo "==> e2e-live: copy over live sshd"
"$BIN" copy "$SRC" "$TARGET" --remote-bin "$BIN" --no-progress
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$REMOTE_DST' --no-progress"

echo "==> e2e-live: unchanged resync should move nothing"
"$BIN" sync "$SRC" "$TARGET" --remote-bin "$BIN" --json --no-progress > "$WORK/noop.json"
python3 - "$WORK/noop.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
sent = r["summary"]["bytes_received"]
assert sent == 0, f"an unchanged resync sent {sent} bytes"
PY

echo "==> e2e-live: sparse update after a small edit"
python3 - "$SRC/model-00001-of-00002.safetensors" <<'PY'
import sys
p = sys.argv[1]
d = bytearray(open(p, "rb").read())
d[2_000_000:2_000_064] = b"FINETUNED" * 7 + b"!"
open(p, "wb").write(bytes(d))
PY

"$BIN" sync "$SRC" "$TARGET" --remote-bin "$BIN" --json --no-progress > "$WORK/delta.json"
python3 - "$WORK/delta.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
total = r["plan"]["total_bytes"]
sent = r["summary"]["bytes_received"]
assert sent < total // 8, f"a 64-byte edit moved {sent} of {total} bytes"
print(f"    live ssh: moved {sent} of {total} bytes")
PY

"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$REMOTE_DST' --no-progress"

echo
echo "e2e-live: all checks passed"
