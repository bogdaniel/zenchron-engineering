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
BASELINE_PIN = "001efd613a40b4b367c6617085e40a24286fac47"
VERIFY_FLAG = "--verify-baseline"


def git(*args):
    return subprocess.check_output(["git", *args])


def paths(*args):
    return {os.fsdecode(p) for p in git("ls-files", "-z", *args).split(b"\0") if p}


SUFFIXES = {
        ".go", ".rs", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
        ".java", ".kt", ".cs", ".php", ".rb", ".swift", ".c", ".h", ".cc",
    ".cpp", ".hpp", ".vue", ".svelte", ".css", ".scss", ".html", ".sql", ".sh",
}


def is_counted_path(name):
    path = PurePosixPath(name)
    return path.suffix in SUFFIXES and not set(path.parts) & {"vendor", "node_modules", ".venv"}


def count_lines(data):
    """Physical line count, or None for generated content."""
    if re.search(
        rb"(?im)^[ \t]*(?://|#|/\*+|\*|<!--|--)[ \t]*(?:code[ \t]+)?"
        rb"generated\b[^\r\n]*\bdo not edit\b", data[:2048]
    ):
        return None
    return data.count(b"\n") + int(bool(data) and not data.endswith(b"\n"))


def count_sources(current):
    counts = {}
    for name in sorted(current):
        if not is_counted_path(name) or Path(name).is_symlink():
            continue
        count = count_lines(Path(name).read_bytes())
        if count is not None:
            counts[name] = count
    return counts


def pinned_count(name):
    """Line count of a regular source file at BASELINE_PIN, or None if it was not one."""
    entry = git("ls-tree", "-z", BASELINE_PIN, "--", name).split(b"\0")[0]
    if not is_counted_path(name) or not entry.startswith((b"100644 blob ", b"100755 blob ")):
        return None
    return count_lines(git("cat-file", "blob", entry.split()[2].decode()))


def verify_baseline(ceilings):
    """Fail closed unless every ceiling is at most the file's line count at BASELINE_PIN."""
    if subprocess.run(
        ["git", "cat-file", "-e", f"{BASELINE_PIN}^{{commit}}"], capture_output=True
    ).returncode:
        raise ValueError(f"baseline pin {BASELINE_PIN} is unavailable (shallow clone?); not verified")
    for name, ceiling in ceilings.items():
        historical = pinned_count(name)
        if historical is None or historical <= HARD_CAP:
            raise ValueError(f"baseline path was not over {HARD_CAP} lines at the pin: {name!r}")
        if ceiling > historical:
            raise ValueError(f"baseline ceiling {ceiling} exceeds {historical} at the pin: {name!r}")
    print(f"baseline verified against {BASELINE_PIN}: {len(ceilings)} entries")


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
    for number, (name, limit, reason) in read_manifest(EXCEPTIONS, tracked, {3}):
        limit = parse_limit(limit, EXCEPTIONS, number)
        if is_stale(name, tracked, counts):
            raise ValueError(f"stale or untracked exception: {name!r}")
        if name in ceilings and limit <= ceilings[name]:
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
    if reason is not None and count <= limit:
        return f"exception {count} {name!r}: {reason}", False
    if name in ceilings and count <= ceilings[name]:
        return f"baseline {count}/{ceilings[name]} {name!r}", False
    if reason is not None:
        return f"FAIL {count} {name!r}: exceeds exception limit {limit}", True
    if name in ceilings:
        return f"FAIL {count} {name!r}: exceeds baseline ceiling {ceilings[name]}", True
    return f"FAIL {count} {name!r}", True


def main(args):
    if set(args) - {VERIFY_FLAG}:
        raise ValueError(f"usage: check_file_sizes.py [{VERIFY_FLAG}]")
    os.chdir(os.fsdecode(git("rev-parse", "--show-toplevel")).rstrip("\n"))
    tracked = paths("--cached")
    current = (tracked | paths("--others", "--exclude-standard")) - paths("--deleted")
    counts = count_sources(current)
    ceilings = load_baseline(tracked, counts)
    if VERIFY_FLAG in args:
        verify_baseline(ceilings)
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
        sys.exit(main(sys.argv[1:]))
    except (OSError, UnicodeError, ValueError, subprocess.CalledProcessError) as error:
        print(f"FAIL file-size check: {error}", file=sys.stderr)
        sys.exit(1)
