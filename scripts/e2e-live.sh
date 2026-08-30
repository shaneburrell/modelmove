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
trap 'rm -rf "$WORK"; "${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "rm -rf /tmp/modelmove-live-$$ /tmp/modelmove-live-resume-$$ /tmp/modelmove-live-fast-$$" >/dev/null 2>&1 || true' EXIT

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

echo "==> e2e-live: corruption is detected and repaired over live sshd"
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "python3 - '$REMOTE_DST/model-00002-of-00002.safetensors'" <<'PY'
import sys
p = sys.argv[1]
d = bytearray(open(p, "rb").read())
d[1_000_000:1_000_032] = b"C" * 32
open(p, "wb").write(bytes(d))
PY
set +e
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$REMOTE_DST' --no-progress" > "$WORK/verify.out" 2>&1
code=$?
set -e
[ "$code" -eq 2 ] || fail "verify exited $code on a corrupt model, want 2"
grep -q "bad chunk" "$WORK/verify.out" || fail "verify did not locate the bad chunk"

"$BIN" sync "$SRC" "$TARGET" --remote-bin "$BIN" --no-progress
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$REMOTE_DST' --no-progress"

echo "==> e2e-live: resume reuses a planted staging file"
RESUME_DST="/tmp/modelmove-live-resume-$$"
RESUME_TARGET="${REMOTE_USER}@${REMOTE_HOST}:${RESUME_DST}"
python3 - "$SRC/model-00001-of-00002.safetensors" "$WORK/partial.part" <<'PY'
import sys
src, dest = sys.argv[1], sys.argv[2]
data = open(src, "rb").read()
partial = bytearray(len(data))
# First 2 MiB is enough for the 4 MiB shard to show reuse without
# needing the FastCDC cut points on this side.
partial[:2_000_000] = data[:2_000_000]
open(dest, "wb").write(partial)
PY
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "mkdir -p '$RESUME_DST/.modelmove/stage'"
scp -o BatchMode=yes -o ConnectTimeout=2 "$WORK/partial.part" \
  "${REMOTE_USER}@${REMOTE_HOST}:${RESUME_DST}/.modelmove/stage/model-00001-of-00002.safetensors.part"
"$BIN" copy "$SRC" "$RESUME_TARGET" --remote-bin "$BIN" --json --no-progress > "$WORK/resume.json"
python3 - "$WORK/resume.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
total = r["plan"]["total_bytes"]
need = r["plan"]["need_bytes"]
assert need < total, f"resume planned {need} of {total} bytes; staged prefix should have been reused"
print(f"    live ssh resume: planned {need} of {total} bytes")
PY
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$RESUME_DST' --no-progress"
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "rm -rf '$RESUME_DST'"

echo "==> e2e-live: --fast skips a same-size in-place edit (verify still catches it)"
FAST_DST="/tmp/modelmove-live-fast-$$"
FAST_TARGET="${REMOTE_USER}@${REMOTE_HOST}:${FAST_DST}"
"$BIN" copy "$SRC" "$FAST_TARGET" --remote-bin "$BIN" --no-progress
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "python3 - '$FAST_DST/model-00002-of-00002.safetensors'" <<'PY'
import os, sys
p = sys.argv[1]
st = os.stat(p)
d = bytearray(open(p, "rb").read())
d[1_000_000] ^= 0xFF
open(p, "wb").write(d)
os.utime(p, (st.st_atime, st.st_mtime))
PY
"$BIN" sync "$SRC" "$FAST_TARGET" --fast --remote-bin "$BIN" --json --no-progress > "$WORK/fast.json"
python3 - "$WORK/fast.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
sent = r["summary"]["bytes_received"]
assert sent == 0, f"--fast sent {sent} bytes after a same-size edit; want 0"
PY
set +e
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "'$BIN' verify '$FAST_DST' --no-progress" > "$WORK/fast-verify.out" 2>&1
code=$?
set -e
[ "$code" -eq 2 ] || fail "verify exited $code after --fast skip, want 2"
"${SSH[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "rm -rf '$FAST_DST'"

echo
echo "e2e-live: all checks passed"
