#!/usr/bin/env python3
"""Run a complete scan and enforce the repository's expiring exception policy."""
import datetime
import json
import pathlib
import subprocess
import sys


def evaluate(data, rules, today):
    """Return refusals; the caller must also verify scanner completion."""
    try:
        exceptions = rules["exceptions"]
        if not isinstance(exceptions, list) or not exceptions:
            raise ValueError("missing exceptions")
        allowed = set()
        for rule in exceptions:
            for field in ("id", "module", "version", "owner", "expires", "reason"):
                if not isinstance(rule[field], str) or not rule[field].strip():
                    raise ValueError("invalid exception metadata")
            if today > datetime.date.fromisoformat(rule["expires"]):
                raise ValueError("exception expired; remove or renew with approval")
            key = (rule["id"], rule["module"], rule["version"])
            if key in allowed:
                raise ValueError("duplicate exception")
            allowed.add(key)
        messages = []
        decoder = json.JSONDecoder()
        remaining = data.strip()
        while remaining:
            message, end = decoder.raw_decode(remaining)
            if not isinstance(message, dict) or len(message) != 1:
                raise ValueError("invalid scan message")
            if next(iter(message)) not in {"config", "SBOM", "progress", "osv", "finding"}:
                raise ValueError("unknown scan message")
            messages.append(message)
            remaining = remaining[end:].lstrip()
        configs = [m["config"] for m in messages if "config" in m]
        sboms = [m["SBOM"] for m in messages if "SBOM" in m]
        if len(configs) != 1 or len(sboms) != 1:
            raise ValueError("scan lacks a unique configuration and inventory")
        config = configs[0]
        if (config.get("protocol_version"), config.get("scan_level"), config.get("scan_mode")) != ("v1.0.0", "symbol", "source"):
            raise ValueError("scan protocol, mode or level not supported")
        sbom = sboms[0]
        if not sbom.get("roots") or not sbom.get("modules"):
            raise ValueError("scan inventory is empty")
        graph = {m["path"]: m.get("version", "") for m in sbom["modules"]}
        if "github.com/labstack/echo/v5" in graph:
            raise ValueError("v5 migration revokes the exception")
        for rule in exceptions:
            if graph.get(rule["module"]) != rule["version"]:
                raise ValueError("exception no longer matches the resolved module graph")
        failures = []
        for message in messages:
            if "finding" not in message:
                continue
            finding = message["finding"]
            frame = finding["trace"][0]
            key = (finding["osv"], frame["module"], frame.get("version", ""))
            function = frame.get("function", "")
            if not isinstance(function, str):
                raise ValueError("invalid finding function")
            if not function:
                level = "package" if frame.get("package") else "module"
                print("Non-called " + level + " finding (informational): " + " ".join(key))
                continue
            if key not in allowed:
                failures.append("unexcepted finding: " + " ".join(key))
        return failures
    except (ValueError, KeyError, TypeError, IndexError, AttributeError) as error:
        return ["scan policy refused: " + str(error)]


def main():
    root = pathlib.Path(__file__).resolve().parents[2]
    try:
        rules = json.loads((root / "scripts/ci/vulnerability-exceptions.json").read_text())
        result = subprocess.run(["govulncheck", "-json", "./..."], cwd=root,
                                stdout=subprocess.PIPE, text=True, check=False)
        # JSON mode reports findings in the stream, not through exit status.
        # Any nonzero status means the scan itself failed, never an exception.
        if result.returncode != 0:
            print("scan did not complete successfully", file=sys.stderr)
            return 1
        failures = evaluate(result.stdout, rules, datetime.datetime.now(datetime.timezone.utc).date())
        for failure in failures:
            print(failure, file=sys.stderr)
        if failures:
            return 1
        print("Scan completed; expiring policy accepted. This is not a finding-free scan.")
        for rule in rules["exceptions"]:
            print(f"Exception: {rule['id']} {rule['module']} {rule['version']}; owner={rule['owner']}; expires={rule['expires']}; {rule['reason']}")
        return 0
    except (OSError, ValueError) as error:
        print("scan unavailable: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
