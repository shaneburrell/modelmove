# Hugging Face Hub workflow

`modelmove` does not talk to the Hugging Face Hub. It moves a model
**directory** between machines. The Hub step is a plain local download with
the `huggingface_hub` CLI; everything after that is `modelmove`.

## 1. Download locally

```bash
pip install -U "huggingface_hub[cli]"

# new CLI
hf download meta-llama/Llama-3-8B --local-dir ./llama-3-8b

# or the legacy entry point
huggingface-cli download meta-llama/Llama-3-8B --local-dir ./llama-3-8b
```

The result is an ordinary directory tree (`.safetensors` shards, config,
tokenizer). That is all `modelmove` needs.

## 2. Transfer

```bash
# First transfer to a new machine
modelmove copy ./llama-3-8b gpu-box:/srv/models/llama-3-8b

# Ship a fine-tune; only the changed tensors move
modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft

# See what it would cost first
modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft --dry-run
```

## 3. Verify

`copy` and `sync` record the manifest they applied in
`<dst>/.modelmove/`, so verification needs no arguments:

```bash
modelmove verify /srv/models/llama-3-8b
```

A mismatch exits with status 2 and names the exact byte ranges that no longer
match. Re-running `sync` repairs exactly those chunks.

## 4. Optional: price a migration with diff

If you already have manifests of two revisions (e.g. a base checkpoint and a
fine-tune), you can compute what a sync would move without touching the
network:

```bash
modelmove manifest ./llama-3-8b > base.manifest
modelmove manifest ./llama-3-8b-ft > finetune.manifest
modelmove diff base.manifest finetune.manifest
```

`diff` runs the same calculation a transfer would, so the numbers are what a
`sync` would actually send.

## Non-goals

- **No `hf://` source.** `modelmove` accepts local paths and SSH destinations
  only. There is no Hub URL scheme and no registry client.
- **No tokens.** Authentication for gated models is handled by the `hf`
  CLI (its own `HF_TOKEN` / login state), not by `modelmove`.
- **No LFS streaming.** Large files are downloaded by the Hub CLI into a
  local directory first; `modelmove` never streams through the Hub.

The lab fixtures follow the same shape: a synthetic tree downloaded (or
created) locally, then moved with `modelmove`.
