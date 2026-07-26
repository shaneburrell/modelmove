#!/usr/bin/env bash
# End-to-end smoke test: build a synthetic sharded model, copy it locally and
# over the SSH transport, make a small edit, and confirm the resync is sparse
# and still verifies.
set -euo pipefail

BIN=${BIN:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/modelmove"}
if [ ! -x "$BIN" ]; then
  echo "e2e: $BIN not found; run 'make build' first" >&2
  exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

SRC="$WORK/src"
mkdir -p "$SRC"

echo "==> building a synthetic Hugging Face checkpoint"
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

# A fake ssh that drops the host argument and runs the command locally. This
# exercises the real subprocess and framing without needing a live sshd.
FAKE_SSH="$WORK/fakessh"
cat > "$FAKE_SSH" <<'EOF'
#!/bin/sh
shift
exec /bin/sh -c "$*"
EOF
chmod +x "$FAKE_SSH"

fail() { echo "e2e: $1" >&2; exit 1; }

echo "==> manifest"
"$BIN" manifest "$SRC" --summary --no-progress > "$WORK/summary.json"
grep -q '"kind": "huggingface"' "$WORK/summary.json" || fail "layout was not detected"
grep -q '"weight_files": 2' "$WORK/summary.json" || fail "weight files were not counted"

echo "==> local copy"
"$BIN" copy "$SRC" "$WORK/local" --no-progress
"$BIN" verify "$WORK/local" --no-progress

echo "==> ssh copy"
"$BIN" copy "$SRC" "fakehost:$WORK/remote" \
  --ssh "$FAKE_SSH" --remote-bin "$BIN" --no-progress
"$BIN" verify "$WORK/remote" --no-progress

echo "==> re-sync with no changes should move nothing"
"$BIN" sync "$SRC" "$WORK/local" --json --no-progress > "$WORK/noop.json"
python3 - "$WORK/noop.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
sent = r["summary"]["bytes_received"]
assert sent == 0, f"an unchanged resync sent {sent} bytes"
PY

echo "==> sparse update after a small edit"
python3 - "$SRC/model-00001-of-00002.safetensors" <<'PY'
import sys
p = sys.argv[1]
d = bytearray(open(p, "rb").read())
d[2_000_000:2_000_064] = b"FINETUNED" * 7 + b"!"
open(p, "wb").write(bytes(d))
PY

for target in "$WORK/local" "fakehost:$WORK/remote"; do
  args=(--json --no-progress)
  case "$target" in
    fakehost:*) args+=(--ssh "$FAKE_SSH" --remote-bin "$BIN") ;;
  esac
  "$BIN" sync "$SRC" "$target" "${args[@]}" > "$WORK/delta.json"
  python3 - "$WORK/delta.json" "$target" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
total = r["plan"]["total_bytes"]
sent = r["summary"]["bytes_received"]
assert sent < total // 8, f"{sys.argv[2]}: a 64-byte edit moved {sent} of {total} bytes"
print(f"    {sys.argv[2]}: moved {sent} of {total} bytes")
PY
done

"$BIN" verify "$WORK/local" --no-progress
"$BIN" verify "$WORK/remote" --no-progress

echo "==> corruption is detected and exits 2"
python3 - "$WORK/local/model-00002-of-00002.safetensors" <<'PY'
import sys
p = sys.argv[1]
d = bytearray(open(p, "rb").read())
d[1_000_000:1_000_032] = b"C" * 32
open(p, "wb").write(bytes(d))
PY
set +e
"$BIN" verify "$WORK/local" --no-progress > "$WORK/verify.out" 2>&1
code=$?
set -e
[ "$code" -eq 2 ] || fail "verify exited $code on a corrupt model, want 2"
grep -q "bad chunk" "$WORK/verify.out" || fail "verify did not locate the bad chunk"

echo "==> repairing the corruption with a sync"
"$BIN" sync "$SRC" "$WORK/local" --no-progress
"$BIN" verify "$WORK/local" --no-progress

echo "==> diff"
"$BIN" manifest "$SRC" --out "$WORK/a.mmm" --format binary --no-progress
"$BIN" diff "$WORK/a.mmm" "$WORK/a.mmm" --exit-code

echo
echo "e2e: all checks passed"
