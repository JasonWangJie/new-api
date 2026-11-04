#!/usr/bin/env python3
"""Run the required database contracts against one disposable local dialect.

External runs require TEST_{MYSQL,POSTGRES}_DSN, its *_LOG_DSN, and
IMAGE_WORKER_TEST_{MYSQL,PG}_DSN. PostgreSQL DSNs must be URLs. The existing
fixtures create/drop tables or isolated databases; never use a shared database.
"""

import argparse
from dataclasses import dataclass
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
from urllib.parse import parse_qsl, unquote, urlsplit


ROOT = Path(__file__).resolve().parents[1]
MODEL_CHANNEL_TESTS = (
    "TestTaskVideoModelBindingWrites",
    "TestChannelOtherSettingsReadOnly",
)
MODEL_POLICY_TESTS = (
    "TestMediaRetryPolicyPersistence",
    "TestMediaRetryRuleOperationTimes",
    "TestMediaRetryPolicyFreshDatabase",
    "TestMediaRetryPolicyLargeDocuments",
)
SERVICE_TESTS = (
    "TestVideoRoutingRewritePreservesPublicRequest",
    "TestVideoRoutingRewriteMultipartPreservesFilesAndOrder",
    "TestVideoRoutingTargetChecksLivePermissionsAndCapability",
    "TestVideoRoutingConfiguredTargetDimensions",
    "TestVideoRoutingConfigurationRevisionAndMatching",
    "TestVideoRoutingPlanFreezesPublicFactsAndTargetOrder",
    "TestVideoRoutingRetryIntersectionAndOriginalFallback",
    "TestVideoRoutingRetryTargetsPreserveSourceRequest",
    "TestVideoRoutingLocalRecoveryRetainsMediaAcrossTargetAndFallback",
    "TestVideoRoutingSourceDefaultsMustBeUnambiguous",
    "TestVideoRoutingPreviewMatchesRuntimeDimensionsAndPermissions",
    "TestVideoRoutingPreviewKnownVideoInputs",
    "TestVideoRoutingAdmissionDefaultsOnlyForPotentialMatches",
    "TestVideoReconciliationChecksFailureEvidence",
    "TestAsyncVideoReconciliationPreservesReservation",
    "TestInlineVideoRecoveryPreparationFailureConsumesOnlyStep",
    "TestMeasuredVideoConcurrentSettlementChargesOnce",
    "TestMeasuredVideoSubscriptionFailureRefundsOnceAndKeepsActualSeconds",
)
CONTROLLER_TESTS = ("TestVideoRoutingManagementAPI", "TestMediaRetryPolicyAPI")


@dataclass(frozen=True)
class Suite:
    name: str
    package: str
    tests: tuple[str, ...]
    dialect: str = ""

    def pattern(self):
        pattern = "^(" + "|".join(self.tests) + ")$"
        return pattern + ("/^" + self.dialect + "$" if self.dialect else "")

    def required(self):
        names = set(self.tests)
        if self.dialect:
            names.update(name + "/" + self.dialect for name in self.tests)
            prefix = "TestTaskVideoModelBindingWrites/" + self.dialect + "/"
            names.update(prefix + pair for pair in (
                "batch_against_batch", "insert_against_update", "insert_against_tag",
                "update_against_tag", "batch_against_upstream_sync",
            ))
        return names


def validate_dsn(dialect, dsn, variable, worker=False):
    """Restrict destructive fixtures to literal loopback and dedicated names."""
    if not dsn:
        raise ValueError(f"{variable} is required for {dialect}")
    if dialect == "mysql":
        match = re.fullmatch(
            r"[^\r\n]*@tcp\((\[[^\]]+\]|[^:()]+):([0-9]+)\)/([a-zA-Z0-9_]+)(?:\?([^\r\n]*))?",
            dsn,
        )
        if not match:
            raise ValueError(f"{variable} must use user[:password]@tcp(loopback:port)/test_database")
        host, port, database, query = match.groups()
        host, port = host.strip("[]"), int(port)
        allowed = {"charset", "parseTime", "loc", "timeout", "readTimeout", "writeTimeout", "collation"}
    else:
        parsed = urlsplit(dsn)
        if parsed.scheme not in ("postgres", "postgresql") or parsed.fragment:
            raise ValueError(f"{variable} must be a PostgreSQL URL")
        host, port = parsed.hostname, parsed.port or 5432
        database, query = unquote(parsed.path.removeprefix("/")), parsed.query
        allowed = {"sslmode", "connect_timeout"}
    try:
        address = ipaddress.ip_address(host)
        host, local = str(address), address.is_loopback
    except ValueError:
        local = False
    if not local or not 1 <= port <= 65535:
        raise ValueError(f"{variable} must point to a literal loopback IP and valid port")
    prefix = "new_api_image_worker_test_" if worker else "new_api_image_test_"
    if not re.fullmatch(re.escape(prefix) + r"[a-zA-Z0-9_]+", database):
        raise ValueError(f"{variable} database must start with {prefix} and contain only letters, digits, underscores")
    parameters = parse_qsl(query or "", keep_blank_values=True, strict_parsing=True)
    if len({key for key, _ in parameters}) != len(parameters) or any(key not in allowed for key, _ in parameters):
        raise ValueError(f"{variable} contains duplicate or unsupported connection parameters")
    return {"host": host, "port": port, "database": database}


def configure_environment(dialect, source):
    env = {
        key: value for key, value in source.items()
        if not (("TEST" in key and (key.endswith("_DSN") or key.endswith("_DIALECT") or key.endswith("_REDIS_ADDR")))
                or key in ("SQL_DSN", "LOG_SQL_DSN", "SQLITE_PATH", "REDIS_CONN_STRING"))
    }
    env.update(GOWORK="off", RETRY_POLICY_TEST_DIALECT=dialect, TEST_TASK_DB_DIALECT=dialect)
    endpoints = []
    if dialect == "sqlite":
        env["RETRY_POLICY_TEST_DSN"] = ""
        return env, endpoints
    tag = "MYSQL" if dialect == "mysql" else "POSTGRES"
    worker_tag = "MYSQL" if dialect == "mysql" else "PG"
    main_key, log_key = f"TEST_{tag}_DSN", f"TEST_{tag}_LOG_DSN"
    worker_key = f"IMAGE_WORKER_TEST_{worker_tag}_DSN"
    for key in (main_key, log_key, worker_key):
        endpoint = validate_dsn(dialect, source.get(key, ""), key, worker=key == worker_key)
        endpoint["variable"] = key
        endpoints.append(endpoint)
        env[key] = source[key]
    if len({item["database"] for item in endpoints}) != len(endpoints):
        raise ValueError("main, log, and worker fixtures require distinct disposable database names")
    env[f"ASYNC_IMAGE_TEST_{worker_tag}_DSN"] = env[main_key]
    env["RETRY_POLICY_TEST_DSN"] = env[main_key]
    return env, endpoints


def evaluate_events(suite, events, returncode):
    started, passed, failed, skipped = set(), set(), set(), set()
    package_passed = False
    version = ""
    for event in events:
        action, name = event.get("Action"), event.get("Test")
        if name:
            target = {"run": started, "pass": passed, "fail": failed, "skip": skipped}.get(action)
            if target is not None:
                target.add(name)
        elif action == "pass":
            package_passed = True
        elif action in ("fail", "skip"):
            (failed if action == "fail" else skipped).add(event.get("Package", suite.package))
        if suite.dialect and name == "TestTaskVideoModelBindingWrites/" + suite.dialect:
            match = re.search(re.escape(suite.dialect) + r" version: ([^\r\n]+)", event.get("Output", ""))
            if match:
                version = match.group(1)
    missing = sorted(suite.required() - (started & passed))
    errors = []
    if returncode:
        errors.append(f"go test exited {returncode}")
    if not package_passed:
        errors.append("package did not finish successfully")
    if failed:
        errors.append("test failures")
    if skipped:
        errors.append("required suite contains skipped tests")
    if missing:
        errors.append("required tests did not run and pass")
    if suite.dialect and not version:
        errors.append("selected database version was not observed")
    return {"suite": suite.name, "passed": len(passed), "failed": sorted(failed),
            "skipped": sorted(skipped), "missing": missing, "errors": errors,
            "database_version": version, "returncode": returncode}


def run_suite(suite, env, output):
    command = ["go", "test", "-p", "2", suite.package, "-run", suite.pattern(), "-count=1", "-timeout=240s", "-json"]
    print(f"Running {suite.name}: {' '.join(command)}", flush=True)
    result = subprocess.run(command, cwd=ROOT, env=env, capture_output=True, text=True, encoding="utf-8", errors="replace")
    (output / (suite.name + ".jsonl")).write_text(result.stdout, encoding="utf-8")
    (output / (suite.name + ".stderr.log")).write_text(result.stderr, encoding="utf-8")
    events = []
    invalid = False
    for line in result.stdout.splitlines():
        try:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError("expected an event object")
            events.append(event)
        except ValueError:
            invalid = True
    summary = evaluate_events(suite, events, result.returncode)
    if invalid:
        summary["errors"].append("go test output contains invalid JSON events")
    print(json.dumps(summary, ensure_ascii=False), flush=True)
    if summary["errors"]:
        print(result.stdout[-12000:] + result.stderr[-4000:], flush=True)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dialect", required=True, choices=("sqlite", "mysql", "postgres"))
    parser.add_argument("--output", type=Path, help="default: .local-tests/database-matrix/<dialect>")
    args = parser.parse_args()
    output = args.output or ROOT / ".local-tests" / "database-matrix" / args.dialect
    output.mkdir(parents=True, exist_ok=True)
    report = {"dialect": args.dialect, "suites": [], "errors": []}
    try:
        env, endpoints = configure_environment(args.dialect, dict(os.environ))
        report["databases"] = endpoints
        for host, port in {(item["host"], item["port"]) for item in endpoints}:
            with socket.create_connection((host, port), timeout=5):
                pass
        suites = (
            Suite("channel-bindings", "./model", MODEL_CHANNEL_TESTS, args.dialect),
            Suite("retry-policy", "./model", MODEL_POLICY_TESTS),
            Suite("video-routing-billing", "./service", SERVICE_TESTS),
            Suite("management-api", "./controller", CONTROLLER_TESTS),
        )
        for suite in suites:
            summary = run_suite(suite, env, output)
            report["suites"].append(summary)
            if summary["errors"]:
                report["errors"].append(suite.name + " failed")
                break
    except (ValueError, OSError) as error:
        report["errors"].append(str(error))
    report["passed"] = sum(suite["passed"] for suite in report["suites"])
    (output / "summary.json").write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False), flush=True)
    return 1 if report["errors"] else 0


if __name__ == "__main__":
    sys.exit(main())
