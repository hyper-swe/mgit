"""Offline refusal tests for the scan policy."""
import copy
import datetime
import importlib.util
import json
import pathlib
import unittest

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("scan_policy", HERE / "check-vulnerabilities.py")
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
MODULE = "github.com/labstack/echo/v4"


def stream(findings):
    messages = [{"config": {"protocol_version": "v1.0.0", "scan_level": "symbol", "scan_mode": "source"}},
                {"SBOM": {"modules": [{"path": MODULE, "version": "v4.16.3"}], "roots": ["github.com/hyper-swe/mgit"]}}]
    messages.extend({"finding": {"osv": advisory, "trace": [{"module": module, "version": version}]}}
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


if __name__ == "__main__":
    unittest.main()
