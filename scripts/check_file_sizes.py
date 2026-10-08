import os
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath

TARGET_LINES = 500
REVIEW_THRESHOLD = 700
HARD_CAP = 1000
EXCEPTIONS = "file-size-exceptions.tsv"
BASELINE = "file-size-baseline.tsv"


def git(*args):
    return subprocess.check_output(["git", *args])


def paths(*args):
    return {os.fsdecode(p) for p in git("ls-files", "-z", *args).split(b"\0") if p}


def count_sources(current):
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
    return counts


def read_manifest(file_name, tracked, column_counts):
    """Yield (line number, fields) for each entry of a tracked, non-symlink TSV manifest."""
    manifest = Path(file_name)
    if manifest.is_symlink():
        raise ValueError(f"{file_name} must not be a symlink")
    if not manifest.exists():
        return
    if file_name not in tracked:
        raise ValueError(f"{file_name} must be tracked")
    seen = set()
    for number, line in enumerate(manifest.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) not in column_counts or not all(field.strip() for field in fields):
            raise ValueError(f"malformed {file_name} entry at line {number}")
        name = fields[0]
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or path.as_posix() != name:
            raise ValueError(f"{file_name} path is not repository-relative: {name!r}")
        if name in seen:
            raise ValueError(f"duplicate {file_name} entry: {name!r}")
        if Path(name).is_symlink():
            raise ValueError(f"{file_name} entry is a symlink: {name!r}")
        seen.add(name)
        yield number, fields


def parse_limit(text, file_name, number):
    if not re.fullmatch(r"[1-9][0-9]*", text) or int(text) <= HARD_CAP:
        raise ValueError(f"{file_name} line {number}: limit must be an integer above {HARD_CAP}")
    return int(text)


def is_stale(name, tracked, counts):
    return name not in tracked or counts.get(name, 0) <= HARD_CAP


def load_baseline(tracked, counts):
    ceilings = {}
    for number, (name, ceiling) in read_manifest(BASELINE, tracked, {2}):
        ceilings[name] = parse_limit(ceiling, BASELINE, number)
        if is_stale(name, tracked, counts):
            raise ValueError(f"stale baseline entry, remove it: {name!r}")
    return ceilings


def load_exceptions(tracked, counts, ceilings):
    exceptions = {}
    for number, fields in read_manifest(EXCEPTIONS, tracked, {2, 3}):
        name, reason = fields[0], fields[-1]
        limit = parse_limit(fields[1], EXCEPTIONS, number) if len(fields) == 3 else None
        if is_stale(name, tracked, counts):
            raise ValueError(f"stale or untracked exception: {name!r}")
        if name in ceilings and (limit is None or limit <= ceilings[name]):
            raise ValueError(f"exception for a baseline file must state a limit above its ceiling: {name!r}")
        exceptions[name] = (limit, reason)
    return exceptions


def classify(name, count, ceilings, exceptions):
    """Return (message or None, failed) for one counted file."""
    if count <= TARGET_LINES:
        return None, False
    if count <= REVIEW_THRESHOLD:
        return f"warn {count} {name!r}", False
    if count <= HARD_CAP:
        return f"review {count} {name!r}", False
    limit, reason = exceptions.get(name, (None, None))
    if reason is not None and (limit is None or count <= limit):
        return f"exception {count} {name!r}: {reason}", False
    if name in ceilings and count <= ceilings[name]:
        return f"baseline {count}/{ceilings[name]} {name!r}", False
    if reason is not None:
        return f"FAIL {count} {name!r}: exceeds exception limit {limit}", True
    if name in ceilings:
        return f"FAIL {count} {name!r}: exceeds baseline ceiling {ceilings[name]}", True
    return f"FAIL {count} {name!r}", True


def main():
    os.chdir(os.fsdecode(git("rev-parse", "--show-toplevel")).rstrip("\n"))
    tracked = paths("--cached")
    current = (tracked | paths("--others", "--exclude-standard")) - paths("--deleted")
    counts = count_sources(current)
    ceilings = load_baseline(tracked, counts)
    exceptions = load_exceptions(tracked, counts, ceilings)
    failed = False
    for name, count in counts.items():
        message, failure = classify(name, count, ceilings, exceptions)
        if message:
            print(message)
        failed |= failure
    return int(failed)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, UnicodeError, ValueError, subprocess.CalledProcessError) as error:
        print(f"FAIL file-size check: {error}", file=sys.stderr)
        sys.exit(1)
