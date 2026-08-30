# modelmove

Sparse-delta, verified transfer and migration of LLM model weights between machines.

[![CI](https://github.com/shaneburrell/modelmove/actions/workflows/ci.yml/badge.svg)](https://github.com/shaneburrell/modelmove/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/shaneburrell/modelmove.svg)](https://pkg.go.dev/github.com/shaneburrell/modelmove)
[![Go Report Card](https://goreportcard.com/badge/github.com/shaneburrell/modelmove)](https://goreportcard.com/report/github.com/shaneburrell/modelmove)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Moving a fine-tune to another box usually means re-sending 70 GB because a few
tensors changed. `modelmove` splits weight files with FastCDC, so a checkpoint
that differs from the one already on the destination only costs the chunks that
actually differ. Every file is staged, checked against a BLAKE3 digest, and
only then swapped into place.

```
$ modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft
source  /home/shane/llama-3-8b-ft
dest    /srv/models/llama-3-8b-ft
model   huggingface llama-3-8b-ft, 12 files, 15.0 GiB (3 weight files, 15.0 GiB), 1 shard set(s)
plan    0 new, 2 updated, 10 unchanged
        412 MiB to send, 14.6 GiB reusable at the destination (97.3% saved)
send  [========================] 100%  412 MiB / 412 MiB  118 MiB/s
sent    412 MiB in 3s (118 MiB/s)
done    2 written, 10 unchanged; 15.0 GiB verified at the destination
```

## Why this exists

`osync` only speaks Ollama. `rsync` has no idea that a `.safetensors` shard set
is one logical object, and its rolling-checksum delta is tuned for text files,
not for 20 GB of high-entropy tensors. Everything else in this space is either
a narrow transplant tool or an enterprise platform.

`modelmove` is a single static binary that treats a multi-gigabyte sharded model
directory as a first-class object:

- **Sparse by default.** Content-defined chunking means an inserted or resized
  tensor shifts bytes without invalidating every chunk after it.
- **Verified before replace.** Files are assembled in `.modelmove/stage`,
  BLAKE3-checked against the source manifest, and only then renamed into place.
  A failed transfer never leaves a half-written shard where a loader can find it.
- **Layout-aware.** Hugging Face, GGUF and Ollama directories are recognised, so
  chunk sizing, transfer order and reporting fit what the files actually are.
- **Resumable without bookkeeping.** Resume works by re-reading and verifying
  what is already staged, so an interrupted run recovers even if the journal,
  the process, or the machine did not survive.

## Install

### Homebrew

```bash
brew install shaneburrell/tap/modelmove
```

The formula is maintained in [`shaneburrell/homebrew-tap`](https://github.com/shaneburrell/homebrew-tap) and tracks GitHub Releases.

### Prebuilt binaries

Download from [Releases](https://github.com/shaneburrell/modelmove/releases) for:

- macOS (Apple Silicon & Intel)
- Linux (amd64 & arm64)
- Windows (amd64)
- FreeBSD (amd64)

### From source

Requires [Go 1.26+](https://go.dev/dl/).

```bash
go install github.com/shaneburrell/modelmove/cmd/modelmove@latest
```

### On the remote host

For SSH transfers, `modelmove` must also be on the remote host's `PATH` — the
local side drives the transfer and the remote side runs `modelmove
remote-helper`. Point `--remote-bin` at it if it lives somewhere unusual:

```bash
scp ~/go/bin/modelmove gpu-box:/usr/local/bin/
```

## Commands

```
modelmove copy <src-dir> <dst>      # copy a model, locally or over SSH
modelmove sync <src-dir> <dst>      # same, plus mirroring with --delete
modelmove verify <dir> [--manifest] # re-hash a directory and check it
modelmove manifest <dir>            # write the content-addressed manifest
modelmove diff <old> <new>          # what a sync between two manifests would move
```

Destinations may be a local path, `host:/path`, `user@host:/path`, or
`ssh://user@host:port/path`.

### copy and sync

```bash
# First transfer to a new machine
modelmove copy ./llama-3-8b gpu-box:/srv/models/llama-3-8b

# Ship a fine-tune; only the changed tensors move
modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft

# See what it would cost first
modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft --dry-run

# Mirror exactly, removing files the source no longer has
modelmove sync ./llama-3-8b /models/llama-3-8b --delete
```

`copy` and `sync` share the same delta engine. The difference is intent: `sync`
supports `--delete` for mirroring, `copy` never removes anything.

### verify

`copy` and `sync` record the manifest they applied in `<dst>/.modelmove/`, so
verification needs no arguments:

```bash
modelmove verify /srv/models/llama-3-8b
```

A mismatch exits with status **2** (a failure to run exits **1**), and the
report names the exact byte ranges that no longer match:

```
checked   5/6 files, 15.0 GiB read
result    FAILED (1 problem file(s))

  model-00002-of-00003.safetensors: digest-mismatch
    want 85940df812d57094a7c4e63aa7f3b2a120c57465fcfebd6b8dd4a9fe0090cb85
    got  6e410bc5b934e45e2cbe95d85342ff9804e99d1bb3d0299187dc9a04b0fb957f
    1 bad chunk(s), 4.0 MiB affected
      offset 1258291200 length 4194304
modelmove: verification failed: 1 of 6 files do not match
```

Re-running `sync` repairs exactly those chunks.

### manifest and diff

```bash
modelmove manifest ./llama-3-8b > model.manifest
modelmove manifest ./llama-3-8b --out model.mmm --format binary
modelmove manifest ./llama-3-8b --summary
modelmove diff base.manifest finetune.manifest
```

`diff` runs the same calculation a transfer would, so you can price a migration
without touching the network:

```
old   base.manifest      (14.6 GiB)
new   finetune.manifest  (15.0 GiB)
files 0 added, 0 removed, 2 modified, 10 unchanged
bytes 412 MiB to transfer, 14.6 GiB reusable (97.3% of the new model already exists)

  modified  model-00002-of-00003.safetensors      388 MiB of    4.9 GiB
  modified  model-00003-of-00003.safetensors       24 MiB of    4.9 GiB
```

Every command takes `--json` for machine-readable output.

## How it works

1. **Scan.** The source directory is walked, classified by layout, and hashed.
   Each file is split with FastCDC (normalized chunking, level 2) and every
   chunk gets a BLAKE3-256 digest. Boundary finding is sequential because it
   has to be; the hashing runs across all cores.
2. **Plan.** The manifest goes to the destination, which hashes what it already
   has and replies with the digests it is missing. Reuse is global: a chunk
   found anywhere at the destination — a different shard, an older revision of
   the same file, a partly staged file from an interrupted run — is never sent.
3. **Stream.** Only the missing chunks cross the network, file by file, with no
   spooling. Each chunk is written to every offset in the file where its digest
   appears.
4. **Assemble and verify.** The destination fills the remaining offsets from its
   own disk, re-reads the staged file, compares its BLAKE3 digest to the
   manifest, and only then renames it over the original.

### Chunk sizing

Chunk size is derived from file role and size, so both machines always agree
without negotiating:

| File | Average chunk |
| --- | --- |
| Weights ≥ 1 GiB | 4 MiB |
| Weights ≥ 64 MiB | 1 MiB |
| Everything else | 64 KiB |

Multi-gigabyte shards get large chunks because a changed tensor spans megabytes,
not kilobytes, and 64 KiB chunks would produce a million manifest entries for no
extra dedupe. Configs and tokenizers stay finely chunked so a one-line edit does
not resend the file. `--avg-size` pins all of it if you want to experiment.

Weight files are not compressed in transit. Quantized and half-precision tensors
are effectively incompressible, so it would only cost CPU.

### Atomicity

`--atomic=file` (default) replaces each file as soon as it verifies, and needs
staging room for one file at a time.

`--atomic=model` stages the whole changed set and swaps it all in at the end, so
the directory is never a mix of old and new shards — useful if something might
try to load the model mid-transfer. It needs staging space for everything that
changed, and in exchange every old file stays available as a chunk source for
the entire run, which finds more reuse when a checkpoint has been re-sharded.

## Flags worth knowing

| Flag | Effect |
| --- | --- |
| `--dry-run`, `-n` | Print the plan and stop |
| `--delete` | Remove destination files the source no longer has (`sync` only) |
| `--fast` | Trust size and mtime instead of re-hashing the destination |
| `--atomic file\|model` | Replace per file, or swap the whole directory at the end |
| `--no-dedupe` | Only reuse chunks from the same path |
| `--no-resume` | Ignore partially staged files from an interrupted run |
| `--exclude` | Glob patterns to skip, repeatable |
| `--jobs`, `-j` | Parallel hashing jobs |
| `--ssh`, `--ssh-option`, `--remote-bin` | Control how the remote helper is reached |
| `--json` | Machine-readable output |

`--fast` is the one to reach for when the destination is a slow disk and you
trust its timestamps. Without it, `modelmove` re-hashes the destination, which
is both the integrity check and the thing that finds reusable chunks.

`--fast` will skip a file whose size and mtime still match the source even if
the bytes have changed (an in-place edit, `touch -r`, some networked
filesystems). That is the contract, not a bug: the next `verify` exits 2, and
a sync without `--fast` repairs the drifted chunks. Run `verify` after a
`--fast` resync if you need to know the destination still matches.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | The command failed |
| `2` | Content mismatch (`verify` found a problem, or `diff --exit-code` found a difference) |

## Development

```bash
make build      # build into bin/
make test       # run the tests
make race       # run them under the race detector
make cover      # coverage, failing below 80%
make lint       # golangci-lint
make fuzz       # short fuzz run over the chunker and the manifest decoder
./scripts/e2e.sh # full round trip over both transports
make check      # everything CI runs
```

## Status and roadmap

The local and SSH transports are complete and covered end to end. SSH
planning-phase manifests are gzip-compressed when both sides advertise
the `manifest-gzip` feature (protocol version stays 1, so older helpers
still work). Planned:

- QUIC transport for high-latency links
- Direct Hugging Face Hub and Ollama registry sources
- Optional compression for metadata *files* in transit, where it actually pays

## License

MIT. See [LICENSE](LICENSE).
