#!/usr/bin/env python3
"""Guard the 2026-11-19 history scrub in live tree content (T152).

The scrub maps private project names to codenames, a private provider
command to `claude-alt`, and the private machine home to `/home/user`.
Later edits reintroduced the private forms at three sites, so this
check fails `make test` when tracked content carries them again.

Two exemptions keep the check honest:

- NOTES.md mapping-history prose names the transformation itself. A
  line inside a `## History scrub` section that names both sides
  ("X became Y") is a record of the mapping, not live content.
- `internal/looper/prices/data.json` keeps the public `zai-org` model
  slugs by decision. The check searches for longer private forms only,
  so public slugs never match.

The private literals are assembled from fragments below, so this file
does not itself carry them.
"""

import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# Private forms, assembled from fragments (see module docstring). The
# value is the public form to use instead; it appears in failure output.
PRIVATE_FORMS = {
    "Bar" + "ista": "the NATO codename (Charlie)",
    "claude-" + "zai": "claude-alt",
    "/home/" + "agent": "/home/user",
}


def mapping_history_lines(notes: Path) -> set:
    """Return NOTES.md line numbers that name a mapping itself.

    A line counts when it sits inside a `## History scrub` section and
    names both sides of a transformation ("X became Y"). Those lines
    are the scrub's own record, not live content.
    """
    allowed = set()
    if not notes.is_file():
        return allowed
    in_section = False
    lines = notes.read_text(encoding="utf-8").splitlines()
    for number, line in enumerate(lines, start=1):
        if line.startswith("## "):
            in_section = line[3:].startswith("History scrub")
            continue
        if in_section and " became " in line:
            allowed.add(number)
    return allowed


def main() -> int:
    allowed = mapping_history_lines(REPO / "NOTES.md")
    failures = []
    for form, replacement in PRIVATE_FORMS.items():
        result = subprocess.run(
            ["git", "-C", str(REPO), "grep", "-n", "-I", "-i", "-e", form],
            capture_output=True,
            text=True,
        )
        if result.returncode not in (0, 1):
            sys.stderr.write(result.stderr)
            return 2
        for hit in result.stdout.splitlines():
            parts = hit.split(":", 2)
            if (
                len(parts) == 3
                and parts[0] == "NOTES.md"
                and parts[1].isdigit()
                and int(parts[1]) in allowed
            ):
                continue
            failures.append("%s    (use %s)" % (hit, replacement))
    if failures:
        sys.stdout.write(
            "Private forms the 2026-11-19 scrub removed are back in "
            "tracked content:\n  %s\n"
            "Rewrite live content with the scrub's public forms. Only a "
            "History scrub section in NOTES.md may name a mapping "
            "itself.\n" % "\n  ".join(failures)
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
