#!/usr/bin/env python3
"""Refresh the pinned Pydantic v2 price snapshot used by the Go binary."""

import argparse
import datetime
import hashlib
import json
from pathlib import Path
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1] / "internal/looper/prices"
REPO = "pydantic/genai-prices"


def download(url):
    with urllib.request.urlopen(url, timeout=30) as response:
        data = response.read(2 << 20)
        if response.read(1):
            raise ValueError("Pricing response exceeds 2 MiB")
        return data


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="main", help="Upstream commit, tag, or branch")
    args = parser.parse_args()
    revision = urllib.parse.quote(args.revision, safe="")
    commit = json.loads(download(f"https://api.github.com/repos/{REPO}/commits/{revision}"))["sha"]
    prefix = f"https://raw.githubusercontent.com/{REPO}/{commit}"
    url = prefix + "/prices/new_data/v2/data.json"
    data = download(url)
    providers = json.loads(data)
    if not isinstance(providers, list) or not {"openai", "anthropic", "zai"}.issubset(
            {provider["id"] for provider in providers}):
        raise ValueError("Unexpected Pydantic v2 provider data")
    license_data = download(prefix + "/LICENSE")
    provenance = {
        "project": REPO, "commit": commit, "url": url,
        "retrieved_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "sha256": hashlib.sha256(data).hexdigest(),
    }
    ROOT.mkdir(parents=True, exist_ok=True)
    (ROOT / "data.json").write_bytes(data)
    (ROOT / "LICENSE").write_bytes(license_data)
    (ROOT / "source.json").write_text(json.dumps(provenance, indent=2) + "\n")
    print(f"Pinned {REPO} at {commit}. Rebuild Looper to use these prices.")


if __name__ == "__main__":
    main()
