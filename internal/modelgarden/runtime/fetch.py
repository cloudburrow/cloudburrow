# CloudBurrow Model Garden serving runtime (#2148), Apache-2.0.
#
# Downloads MODEL_ID from Hugging Face at HF_REVISION into the directory
# given, only the files MODEL_FILES names (glob patterns, one per line) when
# it is set. A gated model's token is HF_TOKEN, from the workload's Secret,
# which huggingface_hub reads itself; it is never printed.
import os
import sys

from huggingface_hub import snapshot_download


def main() -> None:
    target = sys.argv[1]
    patterns = [p for p in os.environ.get("MODEL_FILES", "").splitlines() if p.strip()] or None
    print(f"cloudburrow: downloading {os.environ['MODEL_ID']} at {os.environ['HF_REVISION']}", flush=True)
    snapshot_download(
        repo_id=os.environ["MODEL_ID"],
        revision=os.environ["HF_REVISION"],
        local_dir=target,
        allow_patterns=patterns,
    )
    print("cloudburrow: download complete", flush=True)


if __name__ == "__main__":
    main()
