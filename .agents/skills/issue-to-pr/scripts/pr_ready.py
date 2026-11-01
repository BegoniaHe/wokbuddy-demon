#!/usr/bin/env python3
"""Prepare and check an issue-to-PR workspace for this checkout."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

SCHEMA_VERSION = 1
FORK_REPO = "BegoniaHe/wokbuddy-demon"
RUNS_ROOT = Path(".tmp") / "issue-pr"
PLAN_ROOT = Path(".tmp") / "issue-plan"
SLUG_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
ISSUE_ID_RE = re.compile(r"^issue-\d+$")
LOCAL_ID_RE = re.compile(r"^local-[a-z0-9]+(?:-[a-z0-9]+)*$")
TYPE_TEMPLATES = {
    "feat": "feat.md",
    "fix": "fix.md",
    "docs": "docs.md",
    "refactor": "refactor.md",
    "perf": "refactor.md",
    "style": "refactor.md",
    "build": "chore.md",
    "ops": "chore.md",
    "test": "chore.md",
    "chore": "chore.md",
}
CORE_SECTIONS = (
    "Summary",
    "Related issue",
    "Validation",
    "Compatibility and risk",
    "Checklist",
)
TEMPLATE_SECTIONS = {
    "feat.md": ("Behavior", "Non-goals", "Implementation notes"),
    "fix.md": ("Root cause", "Reproduction", "Implementation notes"),
    "docs.md": ("Files", "Implementation notes"),
    "refactor.md": ("Behavior guarantee", "Implementation notes"),
    "chore.md": ("Surfaces", "Implementation notes"),
}
COMMIT_HEADER_RE = re.compile(
    r"^(feat|fix|refactor|perf|style|test|docs|build|ops|chore)"
    r"(\([a-z0-9][a-z0-9-]*\))?!?:\s+\S"
)
AI_TRAILER_RE = re.compile(r"^AI-Generated:\s*true\s*$", re.IGNORECASE | re.MULTILINE)
GENERATED_AT_RE = re.compile(
    r"^Generated-At:\s*\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\s*$",
    re.MULTILINE,
)
SECRET_PATH_RE = re.compile(
    r"^(?:auths/|data/|config\.json$|\.env|\.tmp/|\.make/"
    r"|server_run[^/]*\.log$|[^/]*\.log$|[^/]*\.(?:pem|key|p12)$)"
)
HUMAN_NOTE_RE = re.compile(r"^##\s+Human note\s*$", re.IGNORECASE | re.MULTILINE)
AGENT_NOTE_RE = re.compile(r"^##\s+Agent note\s*$", re.IGNORECASE | re.MULTILINE)
HEADING_RE = re.compile(r"^##\s+(.+?)\s*$", re.MULTILINE)
FENCE_RE = re.compile(r"```[^\n]*\n(.*?)```", re.DOTALL)
COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)
ISSUE_REF_RE = re.compile(
    r"^\s*(Fixes|Closes|Related):?\s*#\s*$",
    re.IGNORECASE | re.MULTILINE,
)
PLACEHOLDER_RE = re.compile(
    r"\bTBD\b|\bTODO\b|<!--|fill in|add appropriate|implement later",
    re.IGNORECASE,
)
FORBIDDEN_RE = re.compile(
    r"docs\.astrbot\.app|github\.com/(?:Sliverkiss/workbuddy2api"
    r"|misakano7545/workbuddy2api-panel|linguo2625469/workbuddy2api-panel)",
    re.IGNORECASE,
)
STATUS_FILES = ("CONTRACT.md", "PROGRESS.md", "PR.md")


class PrError(RuntimeError):
    """User-facing workspace or readiness error."""


def now_utc() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def repo_root() -> Path:
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "go.mod").is_file() and (parent / "AGENTS.md").is_file():
            return parent
    raise PrError("could not locate the wokbuddy-demon checkout root")


def git(root: Path, *arguments: str) -> str:
    try:
        return subprocess.run(
            ["git", "-C", str(root), *arguments],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.strip() or "git command failed"
        raise PrError(detail) from exc


def run_id_for(*, issue: int | None, slug: str | None) -> str:
    if issue is not None and slug:
        raise PrError("pass either --issue or --slug, not both")
    if issue is not None:
        if issue < 1:
            raise PrError("issue number must be >= 1")
        return f"issue-{issue}"
    if slug:
        if not SLUG_RE.fullmatch(slug):
            raise PrError("slug must be lowercase kebab-case")
        return f"local-{slug}"
    raise PrError("pass --issue or --slug")


def validate_run_id(run_id: str) -> None:
    if not (ISSUE_ID_RE.fullmatch(run_id) or LOCAL_ID_RE.fullmatch(run_id)):
        raise PrError(f"invalid run id: {run_id}")


def write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def load_manifest(run_dir: Path) -> dict[str, Any]:
    path = run_dir / "manifest.json"
    if not path.is_file():
        raise PrError(f"missing {path}")
    payload = json.loads(path.read_text(encoding="utf-8"))
    if payload.get("schema_version") != SCHEMA_VERSION:
        raise PrError("unsupported schema_version")
    return payload


def resolve_run_dir(root: Path, explicit: str | None) -> Path:
    runs_root = root / RUNS_ROOT
    if explicit:
        target = Path(explicit)
        if not target.is_absolute():
            target = root / target
        target = target.resolve()
        if not (target / "manifest.json").is_file():
            raise PrError(f"missing manifest in {target}")
        return target
    pointer = runs_root / "LATEST"
    if pointer.is_file():
        candidate = runs_root / pointer.read_text(encoding="utf-8").strip()
        if (candidate / "manifest.json").is_file():
            return candidate
    raise PrError("no issue-to-PR run exists; run start first")


def infer_type(args_type: str | None, branch: str) -> str | None:
    if args_type:
        if args_type not in TYPE_TEMPLATES:
            raise PrError(f"unknown type: {args_type}")
        return args_type
    prefix = branch.split("/", 1)[0]
    if "/" in branch and prefix in TYPE_TEMPLATES:
        return prefix
    return None


def strip_noise(text: str) -> str:
    return COMMENT_RE.sub("", text)


def template_headings(template: str) -> list[str]:
    cleaned = strip_noise(template)
    return [match.group(1).strip() for match in HEADING_RE.finditer(cleaned)]


def section_body(text: str, title: str) -> str | None:
    matches = list(HEADING_RE.finditer(text))
    for index, match in enumerate(matches):
        if match.group(1).strip().lower() != title.lower():
            continue
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        return text[match.end() : end].strip()
    return None


def build_pr_scaffold(template: str, issue: int | None) -> str:
    cleaned = strip_noise(template)
    cleaned = FENCE_RE.sub("", cleaned)
    parts: list[str] = []
    matches = list(HEADING_RE.finditer(cleaned))
    for index, match in enumerate(matches):
        title = match.group(1).strip()
        if title.lower() == "human note":
            continue
        end = matches[index + 1].start() if index + 1 < len(matches) else len(cleaned)
        body = cleaned[match.end() : end].strip()
        if title.lower() == "related issue" and issue is not None:
            body = ISSUE_REF_RE.sub(
                lambda ref: (
                    f"{ref.group(1)}: #{issue}"
                    if ":" in ref.group(0)
                    else f"{ref.group(1)} #{issue}"
                ),
                body,
            )
            if not body:
                body = f"Related: #{issue}"
        parts.append(f"## {title}\n\n{body}".rstrip())
    if not any(part.lower().startswith("## agent note") for part in parts):
        parts.append("## Agent note\n")
    text = "\n\n".join(parts)
    return re.sub(r"\n{3,}", "\n\n", text).strip() + "\n"


def required_sections(run_type: str) -> list[str]:
    template = TYPE_TEMPLATES[run_type]
    sections = list(CORE_SECTIONS)
    sections.extend(TEMPLATE_SECTIONS.get(template, ("Implementation notes",)))
    if run_type == "perf":
        sections.append("Performance")
    return sections


def cmd_start(args: argparse.Namespace) -> int:
    root = repo_root()
    run_id = run_id_for(issue=args.issue, slug=args.slug)
    validate_run_id(run_id)
    run_dir = root / RUNS_ROOT / run_id
    if run_dir.exists() and not args.force:
        raise PrError(f"{run_dir} already exists; pass --force to replace it")
    branch = git(root, "rev-parse", "--abbrev-ref", "HEAD")
    base = args.base
    if base is None:
        base = default_base(root)
    if base is None:
        raise PrError("no base branch resolves; pass --base")
    if not rev_parses(root, base):
        raise PrError(f"base ref does not resolve: {base}")
    run_type = infer_type(args.type, branch)
    plan_dir = root / PLAN_ROOT / run_id
    plan = None
    if (plan_dir / "manifest.json").is_file():
        plan = str(plan_dir.relative_to(root))
    manifest = {
        "schema_version": SCHEMA_VERSION,
        "run_id": run_id,
        "issue": args.issue,
        "slug": args.slug,
        "repo": FORK_REPO,
        "sha": git(root, "rev-parse", "HEAD"),
        "branch": branch,
        "base": base,
        "type": run_type,
        "plan": plan,
        "created_at_utc": now_utc(),
    }
    scaffold = None
    if run_type:
        template_path = root / ".github" / "PULL_REQUEST_TEMPLATE" / TYPE_TEMPLATES[run_type]
        if not template_path.is_file():
            raise PrError(f"missing PR template: {template_path}")
        scaffold = build_pr_scaffold(
            template_path.read_text(encoding="utf-8"),
            args.issue,
        )
    run_dir.mkdir(parents=True, exist_ok=True)
    write_json(run_dir / "manifest.json", manifest)
    (root / RUNS_ROOT / "LATEST").write_text(run_id + "\n", encoding="utf-8")
    if scaffold is not None:
        (run_dir / "PR.md").write_text(scaffold, encoding="utf-8")
    print(run_dir)
    if not run_type:
        print("warn: no PR type inferred; pass --type before check", file=sys.stderr)
    if not plan and args.issue is not None:
        print("warn: no plan-issue workspace for this run id", file=sys.stderr)
    return 0


def rev_parses(root: Path, ref: str) -> bool:
    try:
        git(root, "rev-parse", "--verify", "--quiet", f"{ref}^{{commit}}")
    except PrError:
        return False
    return True


def default_base(root: Path) -> str | None:
    for ref in ("origin/main", "origin/master", "main", "master"):
        if rev_parses(root, ref):
            return ref
    return None


def changed_files(root: Path, base: str, include_dirty: bool) -> list[str]:
    files = git(root, "diff", "--name-only", f"{base}...HEAD").splitlines()
    if include_dirty:
        for line in git(root, "status", "--porcelain").splitlines():
            name = line[3:].strip()
            if " -> " in name:
                name = name.split(" -> ", 1)[1]
            files.append(name)
    return [item for item in files if item]


def commits_since(root: Path, base: str) -> list[tuple[str, str, str]]:
    payload = git(
        root,
        "log",
        f"--format=%H%x1f%s%x1f%b%x1e",
        f"{base}..HEAD",
    )
    commits: list[tuple[str, str, str]] = []
    for record in payload.split("\x1e"):
        record = record.strip("\n")
        if not record:
            continue
        fields = record.split("\x1f")
        if len(fields) != 3:
            continue
        commits.append((fields[0], fields[1], fields[2]))
    return commits


def check_pr_body(
    text: str,
    run_type: str | None,
    template: str,
    issue: int | None,
) -> list[tuple[str, str]]:
    findings: list[tuple[str, str]] = []
    required = required_sections(run_type) if run_type else list(CORE_SECTIONS)
    if not text.strip():
        return [("block", "PR.md is empty")]
    if HUMAN_NOTE_RE.search(text):
        findings.append(("block", "PR.md carries ## Human note; an agent deletes it"))
    agent_body = section_body(text, "Agent note")
    if agent_body is None:
        findings.append(("block", "PR.md missing ## Agent note"))
    elif len(agent_body.strip()) < 10:
        findings.append(("block", "Agent note is empty"))
    for title in required:
        if title.lower() == "agent note":
            continue
        body = section_body(text, title)
        if body is None:
            findings.append(("block", f"PR.md missing heading: ## {title}"))
        elif not body.strip():
            findings.append(("block", f"PR.md section is empty: ## {title}"))
    optional = [
        title
        for title in template_headings(template)
        if title.lower() not in {section.lower() for section in required}
        and title.lower() not in {"human note", "agent note"}
    ]
    for title in optional:
        body = section_body(text, title)
        if body is None:
            findings.append(("warn", f"PR.md omits optional section: ## {title}"))
        elif not body.strip():
            findings.append(("warn", f"PR.md optional section is empty: ## {title}"))
    if ISSUE_REF_RE.search(text):
        findings.append(("block", "PR.md issue reference is empty (#)"))
    if issue is not None and not re.search(
        rf"(?:Fixes|Closes|Related):?\s*#{issue}\b",
        text,
        re.IGNORECASE,
    ):
        findings.append(("block", f"PR.md does not reference issue #{issue}"))
    validation = section_body(text, "Validation")
    if validation is not None:
        fenced = [
            block
            for block in FENCE_RE.findall(validation)
            if any(line.strip() for line in block.splitlines())
        ]
        if not fenced:
            findings.append(("block", "Validation has no command block"))
    risk = section_body(text, "Compatibility and risk")
    if risk is not None:
        if re.search(r"(?i)\bDoor:\s*(?:one|two)-way\b", risk) is None:
            findings.append(("block", "Compatibility and risk is missing a Door call"))
        if re.search(r"(?i)\bBlast radius:\s*\S", risk) is None:
            findings.append(
                ("block", "Compatibility and risk is missing Blast radius")
            )
    visible = FENCE_RE.sub("", text)
    headings = [match.group(1).strip().lower() for match in HEADING_RE.finditer(visible)]
    if headings and headings[-1] != "agent note":
        findings.append(("block", "Agent note must be the last PR section"))
    if PLACEHOLDER_RE.search(text):
        findings.append(("block", "PR.md still contains template placeholder text"))
    forbidden = FORBIDDEN_RE.search(text)
    if forbidden:
        findings.append(("block", f"forbidden artifact: {forbidden.group(0)}"))
    return findings


def check_commits(
    commits: list[tuple[str, str, str]],
    issue: int | None,
    require_trailer: bool,
) -> list[tuple[str, str]]:
    findings: list[tuple[str, str]] = []
    for sha, subject, body in commits:
        short = sha[:8]
        if not COMMIT_HEADER_RE.match(subject):
            findings.append(("block", f"commit {short} is not Conventional Commits"))
        if require_trailer:
            if not AI_TRAILER_RE.search(body):
                findings.append(("block", f"commit {short} missing AI-Generated: true"))
            if not GENERATED_AT_RE.search(body):
                findings.append(("block", f"commit {short} missing a UTC Generated-At"))
    if issue is not None and commits:
        if not any(re.search(rf"#{issue}\b", body) for _sha, _subject, body in commits):
            findings.append(
                ("warn", f"no commit footer references #{issue}")
            )
    return findings


def check_plan(
    root: Path,
    manifest: dict[str, Any],
    require_plan: bool,
) -> list[tuple[str, str]]:
    plan = manifest.get("plan")
    if not isinstance(plan, str) or not plan:
        level = "block" if require_plan else "warn"
        return [(level, "no plan-issue workspace linked; contract is CONTRACT.md")]
    plan_dir = root / plan
    script = root / ".agents/skills/plan-issue/scripts/issue_plan.py"
    if not (plan_dir / "PLAN.md").is_file() or not script.is_file():
        return [("warn", f"plan pointer is not usable: {plan}")]
    result = subprocess.run(
        [sys.executable, str(script), "--run-dir", str(plan_dir), "validate"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).strip().splitlines()
        reason = detail[0] if detail else "validate failed"
        return [("block", f"plan validation failed: {reason}")]
    return [("ok", "plan validation passed")]


def cmd_check(args: argparse.Namespace) -> int:
    root = repo_root()
    run_dir = resolve_run_dir(root, args.run_dir)
    manifest = load_manifest(run_dir)
    findings: list[tuple[str, str]] = []

    branch = git(root, "rev-parse", "--abbrev-ref", "HEAD")
    if branch in ("main", "master") or branch == "HEAD":
        findings.append(("block", f"refusing to prepare a PR from {branch}"))
    base = str(manifest.get("base") or "")
    if not base or not rev_parses(root, base):
        fallback = default_base(root)
        if fallback is None:
            raise PrError("no base branch resolves; update the manifest base")
        base = fallback
        findings.append(("warn", f"manifest base is unusable; using {base}"))
    try:
        ahead = int(git(root, "rev-list", "--count", f"{base}..HEAD"))
    except PrError:
        ahead = 0
    if ahead == 0:
        findings.append(("block", f"no commits ahead of {base}"))
    else:
        findings.append(("ok", f"{ahead} commit(s) ahead of {base} on {branch}"))

    dirty = bool(git(root, "status", "--porcelain"))
    if dirty and not args.allow_dirty:
        findings.append(("block", "working tree is dirty; commit or stash first"))
    elif dirty:
        findings.append(("warn", "working tree is dirty; only committed changes were checked"))
    else:
        findings.append(("ok", "working tree clean"))

    run_type = args.type or manifest.get("type")
    if run_type and run_type not in TYPE_TEMPLATES:
        findings.append(("block", f"unknown type: {run_type}"))
        run_type = None
    template_text = ""
    if run_type:
        template_path = (
            root / ".github" / "PULL_REQUEST_TEMPLATE" / TYPE_TEMPLATES[run_type]
        )
        if template_path.is_file():
            template_text = template_path.read_text(encoding="utf-8")
        else:
            findings.append(("block", f"missing PR template for {run_type}"))
    else:
        findings.append(("warn", "PR type unknown; pass --type for template checks"))

    for path in changed_files(root, base, args.allow_dirty):
        if SECRET_PATH_RE.match(path):
            findings.append(("block", f"tracked change under a local-only path: {path}"))

    commits = commits_since(root, base)
    findings.extend(
        check_commits(commits, manifest.get("issue"), not args.allow_missing_ai_trailer)
    )

    pr_path = run_dir / "PR.md"
    if not pr_path.is_file():
        findings.append(("block", "missing PR.md"))
    else:
        pr_text = pr_path.read_text(encoding="utf-8")
        findings.extend(
            check_pr_body(pr_text, run_type, template_text, manifest.get("issue"))
        )

    findings.extend(check_plan(root, manifest, args.require_plan))

    try:
        git(root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
    except PrError:
        findings.append(("warn", "branch has no upstream; push before opening the PR"))

    blocks = [message for level, message in findings if level == "block"]
    for level, message in findings:
        print(f"{level:<5} {message}")
    print(f"status: {'BLOCKED' if blocks else 'READY'}")
    return 1 if blocks else 0


def cmd_status(args: argparse.Namespace) -> int:
    root = repo_root()
    run_dir = resolve_run_dir(root, args.run_dir)
    manifest = load_manifest(run_dir)
    files = [name for name in STATUS_FILES if (run_dir / name).is_file()]
    print(
        f"{manifest['run_id']}\t{manifest.get('branch', '')}\t"
        f"{manifest.get('type') or 'unknown'}\t"
        f"{manifest.get('plan') or 'no-plan'}\t"
        f"{','.join(files) or 'none'}\t{run_dir}"
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", help="explicit workspace directory")
    sub = parser.add_subparsers(dest="command", required=True)

    start = sub.add_parser("start", help="create a workspace and PR scaffold")
    start.add_argument("--issue", type=int)
    start.add_argument("--slug")
    start.add_argument("--type")
    start.add_argument("--base")
    start.add_argument("--force", action="store_true")
    start.set_defaults(func=cmd_start)

    check = sub.add_parser("check", help="check PR readiness")
    check.add_argument("--type")
    check.add_argument("--allow-dirty", action="store_true")
    check.add_argument("--allow-missing-ai-trailer", action="store_true")
    check.add_argument("--require-plan", action="store_true")
    check.set_defaults(func=cmd_check)

    status = sub.add_parser("status", help="print workspace status")
    status.set_defaults(func=cmd_status)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except PrError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
