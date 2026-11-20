#!/usr/bin/env python3
"""code-review 的机械门禁：覆盖率账本、发现记录、报告三者一致性。

用法：
    python3 review_ready.py start --run-dir DIR --mode MODE --changed-file FILE
    python3 review_ready.py check --run-dir DIR [--changed-file FILE]

check 只证明结构与一致性；发现是否成立由报告与独立复核负责。
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

MODES = {"worktree", "module", "pr", "range"}
STATUSES = {"pending", "reviewed", "deferred", "out_of_scope", "blocked"}
FINAL_STATUSES = STATUSES - {"pending"}
AXES = {"defect", "standards", "spec", "security"}
VERDICTS = {"confirmed", "needs-validation", "rejected"}
SEVERITIES = {"critical", "important", "minor"}
CONFIDENCES = {"high", "medium", "low"}

COVERAGE_KEYS = {"run_id", "mode", "scope", "base", "head", "changed_files", "units"}
UNIT_KEYS = {"path", "status", "axes", "findings", "checks", "reason"}

COMMON_FINDING_KEYS = {
    "id",
    "fingerprint",
    "axis",
    "file",
    "line",
    "title",
    "verdict",
    "evidence",
    "trigger",
    "impact",
}
OPTIONAL_FINDING_KEYS = {"side", "reviewer"}
SIDES = {"RIGHT", "LEFT", "FILE"}
VERDICT_REQUIRED = {
    "confirmed": COMMON_FINDING_KEYS | {"severity", "fix", "confidence"},
    "needs-validation": COMMON_FINDING_KEYS | {"blockers", "validation"},
    "rejected": COMMON_FINDING_KEYS | {"reason"},
}
VERDICT_ALLOWED = {
    "confirmed": VERDICT_REQUIRED["confirmed"],
    "needs-validation": VERDICT_REQUIRED["needs-validation"],
    "rejected": VERDICT_REQUIRED["rejected"],
}

ID_RE = re.compile(r"^F[0-9]+$")
FINGERPRINT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$")
COMMENT_MARKER = "<!-- code-review:summary -->"


def safe_rel_path(raw: object) -> str:
    """返回 repo 相对路径；绝对路径、`..`、反斜杠、盘符都拒绝。"""
    if not isinstance(raw, str) or not raw.strip():
        raise ValueError("路径为空")
    path = raw.strip()
    if path.startswith(("./", ".\\")):
        path = path[2:]
    if not path:
        raise ValueError("路径为空")
    if path.startswith(("/", "~", "\\")):
        raise ValueError("必须是 repo 相对路径: %s" % raw)
    if "\\" in path or ":" in path:
        raise ValueError("路径含反斜杠或盘符: %s" % raw)
    if any(part in ("", ".", "..") for part in path.split("/")):
        raise ValueError("路径含空段、`.` 或 `..`: %s" % raw)
    return path


def nonempty_str(value: object) -> bool:
    return isinstance(value, str) and bool(value.strip())


def load_json(path: Path, problems: list[str]) -> object:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        problems.append("缺少文件: %s" % path)
    except json.JSONDecodeError as err:
        problems.append("%s 不是合法 JSON: %s" % (path.name, err))
    return None


def load_findings(path: Path, problems: list[str]) -> list[dict]:
    if not path.exists():
        problems.append("缺少文件: %s" % path)
        return []
    records: list[dict] = []
    for lineno, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError as err:
            problems.append("FINDINGS.jsonl:%d 不是合法 JSON: %s" % (lineno, err))
            continue
        if not isinstance(record, dict):
            problems.append("FINDINGS.jsonl:%d 必须是 JSON 对象" % lineno)
            continue
        record["__line"] = lineno
        records.append(record)
    return records


def check_finding(record: dict, changed_files: set[str], problems: list[str]) -> str | None:
    label = "FINDINGS.jsonl:%d" % record["__line"]
    fid = record.get("id")
    if nonempty_str(fid):
        label = "%s (%s)" % (label, fid)

    verdict = record.get("verdict")
    if verdict not in VERDICTS:
        problems.append("%s verdict 必须是 %s" % (label, "/".join(sorted(VERDICTS))))
        return None

    required = VERDICT_REQUIRED[verdict]
    allowed = VERDICT_ALLOWED[verdict]
    missing = sorted(required - record.keys())
    if missing:
        problems.append("%s 缺少字段: %s" % (label, ", ".join(missing)))
    extra = sorted(record.keys() - allowed - OPTIONAL_FINDING_KEYS - {"__line"})
    if extra:
        problems.append("%s 不允许字段: %s" % (label, ", ".join(extra)))

    if "side" in record and record["side"] not in SIDES:
        problems.append("%s side 必须是 %s" % (label, "/".join(sorted(SIDES))))
    if "reviewer" in record and not nonempty_str(record["reviewer"]):
        problems.append("%s reviewer 不能为空" % label)

    if not isinstance(fid, str) or not ID_RE.match(fid):
        problems.append("%s id 必须形如 F1" % label)
    if not nonempty_str(record.get("fingerprint")) or not FINGERPRINT_RE.match(
        record.get("fingerprint", "")
    ):
        problems.append("%s fingerprint 必须是非空稳定根因标识" % label)
    if record.get("axis") not in AXES:
        problems.append("%s axis 必须是 %s" % (label, "/".join(sorted(AXES))))

    file_raw = record.get("file")
    try:
        path = safe_rel_path(file_raw)
    except ValueError as err:
        problems.append("%s file 非法: %s" % (label, err))
        path = None
    if path is not None and path not in changed_files:
        problems.append("%s file 不在变更清单: %s" % (label, path))

    line = record.get("line")
    if isinstance(line, bool) or not isinstance(line, int) or line < 1:
        problems.append("%s line 必须是 >=1 的整数" % label)

    for field in ("title", "evidence", "trigger", "impact"):
        if not nonempty_str(record.get(field)):
            problems.append("%s %s 不能为空" % (label, field))

    if verdict == "confirmed":
        if record.get("severity") not in SEVERITIES:
            problems.append("%s severity 必须是 %s" % (label, "/".join(sorted(SEVERITIES))))
        if record.get("confidence") not in CONFIDENCES:
            problems.append("%s confidence 必须是 %s" % (label, "/".join(sorted(CONFIDENCES))))
        if not nonempty_str(record.get("fix")):
            problems.append("%s fix 不能为空" % label)

    if verdict == "needs-validation":
        blockers = record.get("blockers")
        if not isinstance(blockers, list) or not blockers:
            problems.append("%s needs-validation 必须给出 blockers" % label)
        elif any(not nonempty_str(item) for item in blockers):
            problems.append("%s blockers 条目不能为空" % label)
        if not nonempty_str(record.get("validation")):
            problems.append("%s needs-validation 必须给出 validation 计划" % label)

    if verdict == "rejected" and not nonempty_str(record.get("reason")):
        problems.append("%s rejected 必须给出 reason" % label)

    return fid if isinstance(fid, str) else None


def check_coverage(
    coverage: dict, problems: list[str]
) -> tuple[set[str], list[dict], dict[str, int]]:
    unknown = sorted(coverage.keys() - COVERAGE_KEYS)
    if unknown:
        problems.append("COVERAGE.json 未知顶层字段: %s" % ", ".join(unknown))
    missing = sorted(COVERAGE_KEYS - {"scope", "base", "head"} - coverage.keys())
    if missing:
        problems.append("COVERAGE.json 缺少字段: %s" % ", ".join(missing))

    if coverage.get("mode") not in MODES:
        problems.append("COVERAGE.json mode 必须是 %s" % "/".join(sorted(MODES)))
    if not nonempty_str(coverage.get("run_id")):
        problems.append("COVERAGE.json run_id 不能为空")

    changed: set[str] = set()
    raw_changed = coverage.get("changed_files")
    if not isinstance(raw_changed, list) or not raw_changed:
        problems.append("COVERAGE.json changed_files 必须是非空数组")
    else:
        for raw in raw_changed:
            try:
                path = safe_rel_path(raw)
            except ValueError as err:
                problems.append("changed_files 非法: %s" % err)
                continue
            if path in changed:
                problems.append("changed_files 重复: %s" % path)
            changed.add(path)

    units = coverage.get("units")
    if not isinstance(units, list):
        problems.append("COVERAGE.json units 必须是数组")
        return changed, [], {}

    seen: set[str] = set()
    counts: dict[str, int] = {}
    for index, unit in enumerate(units, 1):
        label = "units[%d]" % index
        if not isinstance(unit, dict):
            problems.append("%s 必须是对象" % label)
            continue
        unknown = sorted(unit.keys() - UNIT_KEYS)
        if unknown:
            problems.append("%s 未知字段: %s" % (label, ", ".join(unknown)))

        try:
            path = safe_rel_path(unit.get("path"))
        except ValueError as err:
            problems.append("%s path 非法: %s" % (label, err))
            continue
        label = "units[%d] %s" % (index, path)
        if path in seen:
            problems.append("%s 重复出现" % label)
        seen.add(path)
        if path not in changed:
            problems.append("%s 不在 changed_files" % label)

        status = unit.get("status")
        if status not in STATUSES:
            problems.append("%s status 必须是 %s" % (label, "/".join(sorted(STATUSES))))
            continue
        counts[status] = counts.get(status, 0) + 1

        axes = unit.get("axes")
        if not isinstance(axes, list):
            problems.append("%s axes 必须是数组" % label)
            axes = []
        else:
            for axis in axes:
                if axis not in AXES:
                    problems.append("%s 未知 axis: %s" % (label, axis))
        if status == "reviewed" and not axes:
            problems.append("%s reviewed 必须记录已应用的 axes" % label)

        findings = unit.get("findings")
        if not isinstance(findings, list):
            problems.append("%s findings 必须是数组" % label)
        else:
            for fid in findings:
                if not isinstance(fid, str) or not ID_RE.match(fid):
                    problems.append("%s findings 含非法 id: %s" % (label, fid))

        checks = unit.get("checks", [])
        if not isinstance(checks, list) or any(not nonempty_str(item) for item in checks):
            problems.append("%s checks 必须是非空字符串数组" % label)

        reason = unit.get("reason", "")
        if status in FINAL_STATUSES and status != "reviewed" and not nonempty_str(reason):
            problems.append("%s %s 必须给出 reason" % (label, status))
        if status == "pending":
            problems.append("%s 仍未审查（status=pending）" % label)

    for path in sorted(changed - seen):
        problems.append("变更文件缺少 coverage 单元: %s" % path)
    return changed, units, counts


def check_links(units: list[dict], finding_ids: set[str], problems: list[str]) -> None:
    linked: set[str] = set()
    for unit in units:
        if not isinstance(unit, dict):
            continue
        for fid in unit.get("findings", []):
            if not isinstance(fid, str):
                continue
            linked.add(fid)
            if fid not in finding_ids:
                problems.append("%s 引用了不存在的发现 %s" % (unit.get("path"), fid))
    for fid in sorted(finding_ids - linked):
        problems.append("发现 %s 没有关联到任何 coverage 单元" % fid)


def check_report(run_dir: Path, finding_ids: set[str], problems: list[str]) -> None:
    path = run_dir / "REPORT.md"
    if not path.exists():
        problems.append("缺少文件: REPORT.md")
        return
    text = path.read_text(encoding="utf-8")
    for heading in ("## 结论", "## 覆盖范围"):
        if heading not in text:
            problems.append("REPORT.md 缺少章节: %s" % heading)
    for fid in sorted(finding_ids):
        if not re.search(r"(?<![A-Za-z0-9])%s(?![0-9])" % re.escape(fid), text):
            problems.append("REPORT.md 未提及发现 %s" % fid)


def check_comment(run_dir: Path, mode: object, problems: list[str]) -> None:
    if mode != "pr":
        return
    path = run_dir / "COMMENT.md"
    if not path.exists():
        problems.append("PR 模式缺少文件: COMMENT.md")
        return
    first = path.read_text(encoding="utf-8").lstrip().splitlines()[:1]
    if not first or first[0].strip() != COMMENT_MARKER:
        problems.append("COMMENT.md 首行必须是 %s" % COMMENT_MARKER)


def check_run(run_dir: Path, changed_file: Path | None = None) -> list[str]:
    problems: list[str] = []
    coverage = load_json(run_dir / "COVERAGE.json", problems)
    if not isinstance(coverage, dict):
        return problems

    changed, units, counts = check_coverage(coverage, problems)
    if changed_file is not None:
        try:
            override = {
                safe_rel_path(line)
                for line in changed_file.read_text(encoding="utf-8").splitlines()
                if line.strip()
            }
        except (OSError, ValueError) as err:
            problems.append("无法读取 --changed-file: %s" % err)
            override = set()
        for path in sorted(override - changed):
            problems.append("实际变更文件未登记在 changed_files: %s" % path)
        for path in sorted(changed - override):
            problems.append("changed_files 多于实际变更: %s" % path)
        changed = override

    records = load_findings(run_dir / "FINDINGS.jsonl", problems)
    finding_ids: set[str] = set()
    fingerprints: set[str] = set()
    verdicts: dict[str, int] = {}
    for record in records:
        fid = check_finding(record, changed, problems)
        if fid is not None:
            if fid in finding_ids:
                problems.append("发现 id 重复: %s" % fid)
            finding_ids.add(fid)
        verdict = record.get("verdict")
        if verdict in VERDICTS:
            verdicts[verdict] = verdicts.get(verdict, 0) + 1
        fingerprint = record.get("fingerprint")
        if nonempty_str(fingerprint):
            if fingerprint in fingerprints:
                problems.append("fingerprint 重复: %s" % fingerprint)
            fingerprints.add(fingerprint)

    check_links(units, finding_ids, problems)
    check_report(run_dir, finding_ids, problems)
    check_comment(run_dir, coverage.get("mode"), problems)

    if not problems:
        reviewed = counts.get("reviewed", 0)
        print(
            "PASS: %d/%d 个单元已审查，%d 条发现（confirmed %d / needs-validation %d / rejected %d）"
            % (
                reviewed,
                len(changed),
                len(records),
                verdicts.get("confirmed", 0),
                verdicts.get("needs-validation", 0),
                verdicts.get("rejected", 0),
            )
        )
    return problems


def read_changed(path: Path) -> list[str]:
    paths: list[str] = []
    seen: set[str] = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        rel = safe_rel_path(line)
        if rel not in seen:
            seen.add(rel)
            paths.append(rel)
    return paths


def cmd_start(args: argparse.Namespace) -> int:
    run_dir: Path = args.run_dir
    try:
        changed = read_changed(args.changed_file)
    except (OSError, ValueError) as err:
        print("FAIL: 无法读取 --changed-file: %s" % err, file=sys.stderr)
        return 2
    if not changed:
        print("FAIL: --changed-file 为空，没有可审查的变更", file=sys.stderr)
        return 2

    run_dir.mkdir(parents=True, exist_ok=True)
    coverage_path = run_dir / "COVERAGE.json"
    if coverage_path.exists():
        print("FAIL: %s 已存在，不覆盖已有账本" % coverage_path, file=sys.stderr)
        return 2

    coverage = {
        "run_id": args.run_id or run_dir.name,
        "mode": args.mode,
        "scope": args.scope or args.mode,
        "base": args.base or "",
        "head": args.head or "",
        "changed_files": changed,
        "units": [
            {
                "path": path,
                "status": "pending",
                "axes": [],
                "findings": [],
                "checks": [],
                "reason": "",
            }
            for path in changed
        ],
    }
    coverage_path.write_text(
        json.dumps(coverage, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    (run_dir / "FINDINGS.jsonl").touch()
    print("wrote %s (%d units pending)" % (coverage_path, len(changed)))
    print("next: 审查每个单元，改 status/axes/findings，再运行 check")
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    problems = check_run(args.run_dir, args.changed_file)
    if not problems:
        return 0
    print("FAIL: %d 个问题" % len(problems), file=sys.stderr)
    for problem in problems:
        print("- %s" % problem, file=sys.stderr)
    return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    start = sub.add_parser("start", help="按变更清单初始化覆盖率账本")
    start.add_argument("--run-dir", type=Path, required=True)
    start.add_argument("--mode", choices=sorted(MODES), required=True)
    start.add_argument("--changed-file", type=Path, required=True)
    start.add_argument("--run-id", default="")
    start.add_argument("--scope", default="")
    start.add_argument("--base", default="")
    start.add_argument("--head", default="")
    start.set_defaults(func=cmd_start)

    check = sub.add_parser("check", help="校验账本、发现与报告一致")
    check.add_argument("--run-dir", type=Path, required=True)
    check.add_argument(
        "--changed-file",
        type=Path,
        help="重新计算实际变更清单并与账本比对",
    )
    check.set_defaults(func=cmd_check)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
