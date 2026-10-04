import os
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath

TARGET_LINES = 500
REVIEW_THRESHOLD = 700
HARD_CAP = 1000


def git(*args):
    return subprocess.check_output(["git", *args])


def paths(*args):
    return {os.fsdecode(p) for p in git("ls-files", "-z", *args).split(b"\0") if p}


def main():
    os.chdir(os.fsdecode(git("rev-parse", "--show-toplevel")).rstrip("\n"))
    tracked = paths("--cached")
    current = (tracked | paths("--others", "--exclude-standard")) - paths("--deleted")
    suffixes = {
        ".go", ".rs", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
        ".java", ".kt", ".cs", ".php", ".rb", ".swift", ".c", ".h", ".cc",
        ".cpp", ".hpp", ".vue", ".svelte", ".css", ".scss", ".html", ".sql", ".sh",
    }
    counts = {}
    for name in sorted(current):
        path = Path(name)
        if path.suffix not in suffixes or set(path.parts) & {"vendor", "node_modules", ".venv"}:
            continue
        if path.is_symlink():
            continue
        data = path.read_bytes()
        if re.search(
            rb"(?im)^[ \t]*(?://|#|/\*+|\*|<!--|--)[ \t]*(?:code[ \t]+)?"
            rb"generated\b[^\r\n]*\bdo not edit\b", data[:2048]
        ):
            continue
        counts[name] = data.count(b"\n") + int(bool(data) and not data.endswith(b"\n"))

    exceptions = {}
    allowlist = Path("file-size-exceptions.tsv")
    if allowlist.is_symlink():
        raise ValueError("the exception file must not be a symlink")
    if allowlist.exists():
        if allowlist.as_posix() not in tracked:
            raise ValueError("the exception file must be tracked")
        for number, line in enumerate(allowlist.read_text(encoding="utf-8").splitlines(), 1):
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            fields = line.split("\t")
            if len(fields) != 2 or not all(field.strip() for field in fields):
                raise ValueError(f"malformed exception at line {number}")
            name, reason = fields
            path = PurePosixPath(name)
            if path.is_absolute() or ".." in path.parts or path.as_posix() != name:
                raise ValueError(f"exception path is not repository-relative: {name!r}")
            if name in exceptions:
                raise ValueError(f"duplicate exception: {name!r}")
            if name not in tracked or counts.get(name, 0) <= HARD_CAP:
                raise ValueError(f"stale or untracked exception: {name!r}")
            exceptions[name] = reason

    failed = False
    for name, count in counts.items():
        if count <= TARGET_LINES:
            continue
        if count <= REVIEW_THRESHOLD:
            print(f"warn {count} {name!r}")
        elif count <= HARD_CAP:
            print(f"review {count} {name!r}")
        elif name in exceptions:
            print(f"exception {count} {name!r}: {exceptions[name]}")
        else:
            print(f"FAIL {count} {name!r}")
            failed = True
    return int(failed)


try:
    sys.exit(main())
except (OSError, UnicodeError, ValueError, subprocess.CalledProcessError) as error:
    print(f"FAIL file-size check: {error}", file=sys.stderr)
    sys.exit(1)
