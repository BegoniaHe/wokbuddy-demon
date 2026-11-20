#!/usr/bin/env python3
"""review_ready.py 的单元测试：行内运行 `python3 -m unittest`。"""

from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import review_ready

GOOD_COVERAGE = {
    "run_id": "pr-7",
    "mode": "pr",
    "scope": "PR #7",
    "base": "aaa",
    "head": "bbb",
    "changed_files": ["internal/pool/pool.go", "web/src/app.tsx", "docs/README.md"],
    "units": [
        {
            "path": "internal/pool/pool.go",
            "status": "reviewed",
            "axes": ["defect", "standards"],
            "findings": ["F1", "F2"],
            "checks": ["go test ./internal/pool/"],
            "reason": "",
        },
        {
            "path": "web/src/app.tsx",
            "status": "reviewed",
            "axes": ["standards"],
            "findings": ["F3"],
            "checks": [],
            "reason": "",
        },
        {
            "path": "docs/README.md",
            "status": "out_of_scope",
            "axes": [],
            "findings": [],
            "checks": [],
            "reason": "文档改动另开 PR 审查",
        },
    ],
}

GOOD_FINDINGS = [
    {
        "id": "F1",
        "fingerprint": "internal/pool/pool.go:cooldown-weights",
        "axis": "defect",
        "file": "internal/pool/pool.go",
        "line": 412,
        "title": "冷却账号仍在候选集",
        "verdict": "confirmed",
        "severity": "critical",
        "evidence": "pickLocked 过滤后未重算 weights",
        "trigger": "全部账号 cooldown 且并发 > 1",
        "impact": "请求落到冷却账号，上游 429 放大",
        "fix": "过滤后重算 weights",
        "confidence": "high",
    },
    {
        "id": "F2",
        "fingerprint": "internal/pool/pool.go:retry-jitter",
        "axis": "defect",
        "file": "internal/pool/pool.go",
        "line": 500,
        "title": "重试抖动可能退化为固定间隔",
        "verdict": "needs-validation",
        "evidence": "rand 源在测试中被固定",
        "trigger": "并发压测下观察重试时间分布",
        "impact": "可能造成上游同时重试",
        "blockers": ["无法在本地复现并发上游行为"],
        "validation": "用假上游记录重试时间戳",
    },
    {
        "id": "F3",
        "fingerprint": "web/src/app.tsx:unused-import",
        "axis": "standards",
        "file": "web/src/app.tsx",
        "line": 12,
        "title": "未使用导入",
        "verdict": "rejected",
        "evidence": "该导入在另一个分支被使用",
        "trigger": "仅在 rebase 前成立",
        "impact": "无",
        "reason": "当前 head 已删除该导入",
    },
]

GOOD_REPORT = """# 代码审查：PR #7

## 结论
需修复：1 个严重问题。

## 覆盖范围
- 已审查 `internal/pool/pool.go`
- 已审查 `web/src/app.tsx`
- 未审查 `docs/README.md`：文档改动另开 PR 审查

## 阻塞问题（严重）
### F1 冷却账号仍在候选集
## 需验证的线索
### F2 重试抖动可能退化为固定间隔
## 已排除的候选
- F3 未使用导入 — 误报原因：当前 head 已删除该导入
"""


class ReviewReadyTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.run_dir = Path(self.tmp.name) / "run"
        self.run_dir.mkdir()
        self.write_run(GOOD_COVERAGE, GOOD_FINDINGS, GOOD_REPORT)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def write_run(self, coverage, findings, report, comment=review_ready.COMMENT_MARKER + "\n## 摘要\n"):
        (self.run_dir / "COVERAGE.json").write_text(
            json.dumps(coverage, ensure_ascii=False), encoding="utf-8"
        )
        (self.run_dir / "FINDINGS.jsonl").write_text(
            "".join(json.dumps(item, ensure_ascii=False) + "\n" for item in findings),
            encoding="utf-8",
        )
        (self.run_dir / "REPORT.md").write_text(report, encoding="utf-8")
        (self.run_dir / "COMMENT.md").write_text(comment, encoding="utf-8")

    def check(self, changed_file: Path | None = None) -> tuple[int, str, str]:
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = review_ready.main(
                ["check", "--run-dir", str(self.run_dir)]
                + (["--changed-file", str(changed_file)] if changed_file else [])
            )
        return code, out.getvalue(), err.getvalue()

    def assert_failure(self, needle: str, changed_file: Path | None = None) -> None:
        code, _, err = self.check(changed_file)
        self.assertEqual(code, 1, err)
        self.assertIn(needle, err)

    def test_valid_run_passes(self) -> None:
        code, out, err = self.check()
        self.assertEqual(code, 0, err)
        self.assertIn("PASS", out)
        self.assertIn("confirmed 1 / needs-validation 1 / rejected 1", out)

    def test_optional_side_and_reviewer_accepted(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[0]["side"] = "RIGHT"
        findings[0]["reviewer"] = "explore:refute-F1 + explore:record-F1"
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        code, _, err = self.check()
        self.assertEqual(code, 0, err)

    def test_invalid_side_fails(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[0]["side"] = "right"
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("side 必须是")

    def test_pending_unit_fails(self) -> None:
        coverage = json.loads(json.dumps(GOOD_COVERAGE))
        coverage["units"][0]["status"] = "pending"
        self.write_run(coverage, GOOD_FINDINGS, GOOD_REPORT)
        self.assert_failure("status=pending")

    def test_missing_unit_fails(self) -> None:
        coverage = json.loads(json.dumps(GOOD_COVERAGE))
        coverage["units"] = coverage["units"][:1]
        self.write_run(coverage, GOOD_FINDINGS, GOOD_REPORT)
        self.assert_failure("缺少 coverage 单元: web/src/app.tsx")

    def test_path_traversal_fails(self) -> None:
        coverage = json.loads(json.dumps(GOOD_COVERAGE))
        coverage["changed_files"].append("../etc/passwd")
        self.write_run(coverage, GOOD_FINDINGS, GOOD_REPORT)
        self.assert_failure("路径含空段")

    def test_severity_on_needs_validation_fails(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[1]["severity"] = "important"
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("不允许字段: severity")

    def test_needs_validation_needs_blockers(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[1]["blockers"] = []
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("必须给出 blockers")

    def test_rejected_needs_reason(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[2]["reason"] = ""
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("rejected 必须给出 reason")

    def test_duplicate_fingerprint_fails(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[2]["fingerprint"] = findings[0]["fingerprint"]
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("fingerprint 重复")

    def test_dot_directory_fingerprint_accepted(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[0]["fingerprint"] = ".agents/skills/x/SKILL.md:stale-convention"
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        code, _, err = self.check()
        self.assertEqual(code, 0, err)

    def test_punctuation_only_fingerprint_fails(self) -> None:
        findings = json.loads(json.dumps(GOOD_FINDINGS))
        findings[0]["fingerprint"] = ".:"
        self.write_run(GOOD_COVERAGE, findings, GOOD_REPORT)
        self.assert_failure("fingerprint 必须是非空稳定根因标识")

    def test_unlinked_finding_fails(self) -> None:
        coverage = json.loads(json.dumps(GOOD_COVERAGE))
        coverage["units"][1]["findings"] = []
        self.write_run(coverage, GOOD_FINDINGS, GOOD_REPORT)
        self.assert_failure("发现 F3 没有关联到任何 coverage 单元")

    def test_report_missing_finding_fails(self) -> None:
        self.write_run(
            GOOD_COVERAGE,
            GOOD_FINDINGS,
            GOOD_REPORT.replace("### F2", "### 重试抖动"),
        )
        self.assert_failure("REPORT.md 未提及发现 F2")

    def test_changed_file_override_fails_on_drift(self) -> None:
        changed = Path(self.tmp.name) / "changed.txt"
        changed.write_text("internal/pool/pool.go\nweb/src/app.tsx\nnew/file.go\n", encoding="utf-8")
        self.assert_failure("实际变更文件未登记", changed)

    def test_non_object_coverage_fails(self) -> None:
        (self.run_dir / "COVERAGE.json").write_text("[]", encoding="utf-8")
        self.assert_failure("顶层必须是 JSON 对象")

    def test_unreadable_report_fails(self) -> None:
        (self.run_dir / "REPORT.md").write_bytes(b"\xff\xfe")
        self.assert_failure("无法以 UTF-8 读取")

    def test_comment_marker_required_for_pr(self) -> None:
        self.write_run(GOOD_COVERAGE, GOOD_FINDINGS, GOOD_REPORT, comment="## 摘要\n")
        self.assert_failure("COMMENT.md 首行")

    def test_start_scaffolds_pending_and_check_blocks(self) -> None:
        run = Path(self.tmp.name) / "started"
        changed = Path(self.tmp.name) / "changed.txt"
        changed.write_text("a.go\nb.go\n", encoding="utf-8")
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = review_ready.main(
                [
                    "start",
                    "--run-dir",
                    str(run),
                    "--mode",
                    "range",
                    "--changed-file",
                    str(changed),
                ]
            )
        self.assertEqual(code, 0, err)
        ledger = json.loads((run / "COVERAGE.json").read_text(encoding="utf-8"))
        self.assertEqual([unit["status"] for unit in ledger["units"]], ["pending", "pending"])

        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = review_ready.main(["check", "--run-dir", str(run)])
        self.assertEqual(code, 1)


if __name__ == "__main__":
    unittest.main()
