"""Offline refusal tests for the scan policy."""
import copy
import contextlib
import io
import datetime
import importlib.util
import json
import pathlib
import os
import shutil
import subprocess
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("scan_policy", HERE / "check-vulnerabilities.py")
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
MODULE = "github.com/labstack/echo/v4"


def stream(findings):
    messages = [{"config": {"protocol_version": "v1.0.0", "scan_level": "symbol", "scan_mode": "source"}},
                {"SBOM": {"modules": [{"path": MODULE, "version": "v4.16.3"}], "roots": ["github.com/hyper-swe/mgit"]}}]
    messages.extend({"finding": {"osv": advisory, "trace": [{"module": module, "version": version, "package": module + "/example", "function": "Called"}]}}
                    for advisory, module, version in findings)
    return "\n".join(json.dumps(m) for m in messages)


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.rules = json.loads((HERE / "vulnerability-exceptions.json").read_text())
        self.today = datetime.date(2026, 11, 9)
        self.findings = [(i, MODULE, "v4.16.3") for i in ("GO-2026-6872", "GO-2026-6875")]

    def check(self, data=None, rules=None, today=None):
        return policy.evaluate(data if data is not None else stream(self.findings),
                               rules if rules is not None else self.rules, today or self.today)

    def test_only_exact_approved_findings_pass(self):
        self.assertEqual([], self.check())
        self.assertEqual([], self.check(stream([])))
        self.assertEqual({"GO-2026-6872", "GO-2026-6875"}, {r["id"] for r in self.rules["exceptions"]})
        for r in self.rules["exceptions"]:
            self.assertEqual(MODULE, r["module"])
            self.assertEqual("v4.16.3", r["version"])
            self.assertEqual("2026-12-09", r["expires"])
            self.assertEqual("mgit maintainer", r["owner"])
            self.assertTrue(r["reason"].strip())

    def test_unexcepted_finding_is_red(self):
        self.assertTrue(self.check(stream(self.findings + [("GO-2099-9999", MODULE, "v4.16.3")])))

    def test_non_called_findings_are_green_and_logged(self):
        for package in (None, "example.org/module/pkg"):
            frame = {"module": "example.org/module", "version": "v1.2.3"}
            if package is not None:
                frame["package"] = package
            data = stream(self.findings) + "\n" + json.dumps({"finding": {"osv": "GO-2099-9999", "trace": [frame]}})
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                self.assertEqual([], self.check(data))
            self.assertIn("GO-2099-9999", output.getvalue())
            self.assertIn("example.org/module v1.2.3", output.getvalue())
            self.assertIn("package" if package else "module", output.getvalue())
        # The same unexcepted advisory is blocking as soon as it is called.
        called = stream(self.findings + [("GO-2099-9999", MODULE, "v4.16.3")])
        self.assertTrue(self.check(called))

    def test_other_module_or_version_is_red(self):
        for module, version in [(MODULE, "v4.16.2"), (MODULE, "v4.16.4"), (MODULE, ""),
                                ("github.com/labstack/echo/v5", "v4.16.3"), ("other", "v4.16.3")]:
            with self.subTest(module=module, version=version):
                self.assertTrue(self.check(stream([("GO-2026-6872", module, version)])))

    def test_expiry_fails_even_without_findings(self):
        self.assertEqual([], self.check(today=datetime.date(2026, 12, 9)))
        self.assertTrue(self.check(stream([]), today=datetime.date(2026, 12, 10)))

    def test_v5_or_changed_inventory_revokes_exception(self):
        for old, new in [(MODULE, "github.com/labstack/echo/v5"), ("v4.16.3", "v4.16.4")]:
            # Even a stale finding cannot authorize a different resolved graph.
            data = stream(self.findings)
            first, rest = data.split('\n', 1)
            sbom, findings = rest.split('\n', 1)
            self.assertTrue(self.check(first + '\n' + sbom.replace(old, new) + '\n' + findings))

    def test_v5_alongside_v4_revokes_exception(self):
        data = stream(self.findings).replace('"roots":', '"modules_extra": [], "roots":')
        messages = [json.loads(line) for line in data.splitlines()]
        messages[1]["SBOM"]["modules"].append({"path": "github.com/labstack/echo/v5", "version": "v5.0.0"})
        self.assertTrue(self.check("\n".join(json.dumps(m) for m in messages)))

    def test_incomplete_or_malformed_scan_fails_closed(self):
        for data in ["", "{}", "not json", stream([]) + '{', '{"finding": {}}',
                     stream([]).replace('"symbol"', '"package"'),
                     stream([]).replace('"v1.0.0"', '"v9.0.0"')]:
            with self.subTest(data=data):
                self.assertTrue(self.check(data))

    def test_bad_policy_fails_closed(self):
        for field in ("id", "module", "version", "owner", "expires", "reason"):
            rules = copy.deepcopy(self.rules)
            rules["exceptions"][0][field] = ""
            self.assertTrue(self.check(rules=rules))
        rules = copy.deepcopy(self.rules)
        rules["exceptions"][0]["expires"] = "never"
        self.assertTrue(self.check(rules=rules))

    def test_real_cli_preserves_scanner_failure_and_rejects_findings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            target = root / "scripts/ci"
            target.mkdir(parents=True)
            shutil.copyfile(HERE / "check-vulnerabilities.py", target / "check-vulnerabilities.py")
            rules = copy.deepcopy(self.rules)
            for rule in rules["exceptions"]:
                rule["expires"] = (datetime.datetime.now(datetime.timezone.utc).date() + datetime.timedelta(days=1)).isoformat()
            (target / "vulnerability-exceptions.json").write_text(json.dumps(rules))
            scanner = root / "govulncheck"
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"])
            cases = [(stream(self.findings), 0, 0),
                     (stream(self.findings + [("GO-2099-9999", MODULE, "v4.16.3")]), 0, 1),
                     (stream(self.findings), 1, 1), (stream(self.findings), 3, 1),
                     ("", 0, 1), (stream(self.findings) + "{", 0, 1)]
            for data, status, want in cases:
                scanner.write_text("#!/bin/sh\n[ \"$*\" = \"-json ./...\" ] || exit 90\ncat <<'DATA'\n" + data + "\nDATA\nexit " + str(status) + "\n")
                scanner.chmod(0o755)
                result = subprocess.run(["python3", str(target / "check-vulnerabilities.py")], env=env, capture_output=True, text=True)
                self.assertEqual(want, result.returncode, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
