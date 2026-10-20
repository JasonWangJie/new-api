#!/usr/bin/env python3
"""Regression tests using fake services; never connects to Docker or a database."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest


TOOLS_DIR = Path(__file__).resolve().parents[1]
OLD_SHA = "a" * 40
NEW_SHA = "b" * 40
OLD_IMAGE = f"new-api-fork:{OLD_SHA}"
NEW_IMAGE = f"new-api-fork:{NEW_SHA}"
OLD_ID = "sha256:" + "1" * 64
NEW_ID = "sha256:" + "2" * 64


def fake_command(tool, arguments):
    root = Path(os.environ["TEST_ROOT"])
    state_path = root / "state.json"
    state = json.loads(state_path.read_text(encoding="utf-8"))
    args = list(arguments)
    if tool == "docker" and args[0] == "compose":
        args.pop(0)
        while args and args[0].startswith("-"):
            option = args.pop(0)
            if option not in ("--project-name", "--project-directory", "--env-file", "-f"):
                raise RuntimeError(f"Unexpected compose option: {option}")
            args.pop(0)
        tool = "compose"
    with (root / "events.jsonl").open("a", encoding="utf-8") as events:
        events.write(json.dumps([tool, *args]) + "\n")

    def save():
        temporary = state_path.with_suffix(f".{os.getpid()}.tmp")
        temporary.write_text(json.dumps(state), encoding="utf-8")
        temporary.replace(state_path)

    if tool == "flock":
        return int(state.get("lock_fail", False))
    if tool == "git":
        if args[:2] == ["-C", "src"]:
            args = args[2:]
        if args == ["remote", "get-url", "origin"]:
            print("https://github.com/JasonWangJie/new-api.git")
        elif args == ["branch", "--show-current"]:
            print("main")
        elif args == ["status", "--porcelain"]:
            pass
        elif args == ["fetch", "origin", "main"]:
            pass
        elif args == ["merge", "--ff-only", "origin/main"]:
            state["head"] = state["next_sha"]
            save()
        elif args == ["rev-parse", "HEAD"]:
            print(state["head"])
        elif args == ["rev-parse", "origin/main"]:
            print(state["next_sha"])
        else:
            raise RuntimeError(f"Unexpected git command: {args}")
        return 0
    if tool == "docker":
        if args == ["buildx", "version"]:
            return int(not state.get("buildx_available", True))
        if args in (["buildx", "prune", "--all", "--force"], ["builder", "prune", "--all", "--force"]):
            return int(state.get("prune_fail", False))
        if args[0] == "inspect":
            template = args[args.index("--format") + 1]
            if template == "{{.Config.Image}}":
                print(state["active_image"])
            elif template == "{{.Image}}":
                print(state["active_id"])
            else:
                raise RuntimeError(f"Unexpected container inspection: {template}")
            return 0
        if args[:2] == ["image", "inspect"]:
            template = args[args.index("--format") + 1]
            if template != "{{.Id}}":
                raise RuntimeError(f"Unexpected image inspection: {template}")
            image = args[-1]
            if image in (OLD_IMAGE, OLD_ID):
                print(OLD_ID)
            elif image in (NEW_IMAGE, NEW_ID):
                print(state.get("built_id", NEW_ID))
            else:
                raise RuntimeError(f"Unexpected image: {image}")
            return 0
        raise RuntimeError(f"Unexpected docker command: {args}")
    if tool == "compose":
        command = args[0]
        if command == "config":
            if "--format" in args:
                settings = dict(line.split("=", 1) for line in (root / "image.env").read_text(encoding="utf-8").splitlines())
                print(json.dumps({"services": {"new-api": {"image": f'{settings["APP_IMAGE_REPOSITORY"]}:{settings["APP_IMAGE_TAG"]}'}}}))
            return 0
        if command == "ps":
            if state["app_running"]:
                print(state["container"])
            return 0
        if command == "build":
            state["build_repository"] = os.environ.get("APP_IMAGE_REPOSITORY")
            state["build_tag"] = os.environ.get("APP_IMAGE_TAG")
            save()
            if state.get("build_fail"):
                return 1
            state["built_id"] = NEW_ID
            save()
            return 0
        if command == "stop":
            state["app_running" if args[-1] == "new-api" else "redis_running"] = False
            save()
            return 0
        if command == "start":
            state["app_running" if args[-1] == "new-api" else "redis_running"] = True
            save()
            return 0
        if command == "exec" and "pg_dump" in args:
            if state.get("dump_fail"):
                return 1
            print("Fake PostgreSQL archive")
            return 0
        if command == "exec" and "pg_restore" in args:
            sys.stdin.read()
            print("Fake archive contents")
            return 0
        if command == "exec" and "redis" in args:
            print("OK")
            return 0
        if command == "up":
            service = args[-1]
            if service == "redis":
                if state.get("redis_up_fail"):
                    return 1
                state["redis_running"] = True
            elif service == "new-api":
                if state.get("app_up_fail"):
                    return 1
                state.update(app_running=True, active_image=NEW_IMAGE, active_id=NEW_ID)
            else:
                raise RuntimeError(f"Unexpected compose service: {service}")
            save()
            return 0
        if command == "port":
            print("127.0.0.1:4040")
            return 0
        raise RuntimeError(f"Unexpected compose command: {args}")
    if tool == "curl":
        if "http://127.0.0.1:4040/api/status" not in args:
            raise RuntimeError(f"Health check used the wrong port: {args}")
        if state.get("health_fail"):
            return 22
        print('{"success":true}')
        return 0
    raise RuntimeError(f"Unexpected fake command: {tool}")


class UpdateConfirmTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bash = shutil.which("bash")
        if os.name == "nt":
            git_bash = Path("C:/Program Files/Git/usr/bin/bash.exe")
            if git_bash.is_file():
                cls.bash = str(git_bash)
        if not cls.bash:
            raise unittest.SkipTest("Bash is required")
        if not (TOOLS_DIR / "update-confirm.sh").is_file():
            raise unittest.SkipTest("update-confirm.sh has not been created")

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="update-confirm-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for directory in ("bin", "src", "data", "logs", "images", "redis-data"):
            (self.root / directory).mkdir()
        for filename in ("compose-env.sh", "compose.fork.yml", "manage.sh", "update-confirm.sh"):
            shutil.copyfile(TOOLS_DIR / filename, self.root / filename)
        (self.root / ".env").write_text("NEWAPI_PORT=4040\n", encoding="utf-8")
        self.original_image_env = f"APP_IMAGE_TAG={OLD_SHA}\nAPP_IMAGE_REPOSITORY=new-api-fork\n"
        (self.root / "image.env").write_text(self.original_image_env, encoding="utf-8")
        (self.root / "data" / "retained.txt").write_text("existing runtime data", encoding="utf-8")
        self.initial_state = {
            "head": OLD_SHA, "next_sha": NEW_SHA, "container": "container-old",
            "active_image": OLD_IMAGE, "active_id": OLD_ID,
            "app_running": True, "redis_running": True,
        }
        self.write_state(self.initial_state)
        wrapper = '#!/usr/bin/env bash\nexec "$TEST_PYTHON" "$TEST_DRIVER" --fake "${0##*/}" "$@"\n'
        for command in ("docker", "git", "flock", "curl"):
            path = self.root / "bin" / command
            path.write_text(wrapper, encoding="utf-8", newline="\n")
            path.chmod(0o755)
        python3 = self.root / "bin" / "python3"
        python3.write_text('#!/usr/bin/env bash\nexec "$TEST_PYTHON" "$@"\n', encoding="utf-8", newline="\n")
        python3.chmod(0o755)
        deployment_directory = self.root.as_posix()
        if os.name == "nt":
            deployment_directory = "/" + deployment_directory[0].lower() + deployment_directory[2:]
        self.environment = dict(os.environ, TEST_ROOT=self.root.as_posix(),
                                TEST_PYTHON=Path(sys.executable).as_posix(),
                                TEST_DRIVER=Path(__file__).resolve().as_posix(),
                                NEWAPI_DEPLOY_DIR=deployment_directory)
        self.environment["PATH"] = str(self.root / "bin") + os.pathsep + os.environ["PATH"]
        self.environment.pop("APP_IMAGE_TAG", None)
        self.environment.pop("APP_IMAGE_REPOSITORY", None)
        self.process = None
        self.output_file = (self.root / "output.txt").open("wb")
        self.addCleanup(self.output_file.close)
        self.addCleanup(self.stop_process)

    def write_state(self, state):
        temporary = self.root / f"state.{os.getpid()}.tmp"
        temporary.write_text(json.dumps(state), encoding="utf-8")
        temporary.replace(self.root / "state.json")

    def state(self):
        return json.loads((self.root / "state.json").read_text(encoding="utf-8"))

    def change_state(self, **changes):
        state = self.state()
        state.update(changes)
        self.write_state(state)

    def events(self):
        path = self.root / "events.jsonl"
        return [json.loads(line) for line in path.read_text(encoding="utf-8").split("\n")[:-1]] if path.exists() else []

    def output(self):
        return (self.root / "output.txt").read_text(encoding="utf-8", errors="replace")

    def start(self, entrypoint="confirm"):
        self.stop_process()
        self.output_file.seek(0)
        self.output_file.truncate()
        (self.root / "events.jsonl").write_text("", encoding="utf-8")
        state = self.state()
        state.pop("built_id", None)
        self.write_state(state)
        bin_directory = '"$(cygpath -u "$TEST_ROOT")/bin"' if os.name == "nt" else '"$TEST_ROOT/bin"'
        script = "update-confirm.sh" if entrypoint == "confirm" else "manage.sh"
        arguments = "" if entrypoint == "confirm" else " update --ready"
        launcher = (f'export PATH={bin_directory}:"$PATH"; '
                    'printf "%s" "$BASHPID" > "$TEST_ROOT/bash.pid"; '
                    f'exec /usr/bin/bash "$NEWAPI_DEPLOY_DIR/{script}"{arguments}')
        self.process = subprocess.Popen([self.bash, "-c", launcher], stdin=subprocess.PIPE,
                                        stdout=self.output_file, stderr=subprocess.STDOUT,
                                        env=self.environment)

    def wait_for_confirmation(self):
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            if "UPDATE" in self.output() and any(event[:2] == ["compose", "build"] for event in self.events()):
                self.assertIsNone(self.process.poll(), self.output())
                return
            if self.process.poll() is not None:
                self.fail(f"Script exited before confirmation:\n{self.output()}")
            threading.Event().wait(0.02)
        self.fail(f"Script did not reach confirmation:\n{self.output()}")

    def finish(self, answer=b"UPDATE\n"):
        self.process.communicate(answer, timeout=45)
        return self.process.returncode

    def stop_process(self):
        if self.process and self.process.poll() is None:
            if os.name == "nt":
                subprocess.run(["taskkill", "/PID", str(self.process.pid), "/T", "/F"], capture_output=True)
            else:
                self.process.terminate()
            self.process.wait(timeout=5)
        if self.process and self.process.stdin:
            self.process.stdin.close()

    def assert_no_service_changes(self):
        changes = [event for event in self.events() if event[:1] == ["compose"] and event[1] in ("stop", "start", "up")]
        self.assertEqual([], changes, self.output())
        self.assertEqual(self.original_image_env, (self.root / "image.env").read_text(encoding="utf-8"))
        self.assertFalse((self.root / "backups").exists(), self.output())
        self.assert_no_cache_cleanup()

    def assert_no_cache_cleanup(self):
        self.assertFalse(any(event[0] == "docker" and "prune" in event for event in self.events()), self.output())

    def reset_deployment(self, entrypoint, **changes):
        self.write_state({**self.initial_state, **changes})
        (self.root / "image.env").write_text(self.original_image_env, encoding="utf-8")
        backups = self.root / "backups"
        if backups.exists():
            shutil.rmtree(backups)
        if entrypoint == "ready":
            backups.mkdir()
        self.environment["NEWAPI_PORT"] = "4040"

    def test_build_waits_for_confirmation(self):
        self.start()
        self.wait_for_confirmation()
        self.assert_no_service_changes()
        self.assertTrue(self.state()["app_running"])
        self.finish(b"cancel\n")
        self.assert_no_service_changes()

    def test_only_complete_exact_confirmation_is_accepted(self):
        for answer in (b"", b"\n", b"update\n", b"UPDATE \n", b"UPDATE"):
            with self.subTest(answer=answer):
                self.start()
                self.wait_for_confirmation()
                self.finish(answer)
                self.assert_no_service_changes()

    def test_signals_before_confirmation_do_not_stop_services(self):
        for signal in ("INT", "TERM", "HUP"):
            with self.subTest(signal=signal):
                self.start()
                self.wait_for_confirmation()
                result = subprocess.run([self.bash, "-c", 'read -r test_pid < "$TEST_ROOT/bash.pid"; kill -s "$1" "$test_pid"', "test", signal],
                                        env=self.environment, capture_output=True)
                self.assertEqual(0, result.returncode, result.stderr)
                self.process.wait(timeout=5)
                self.assertNotEqual(0, self.process.returncode, self.output())
                self.assert_no_service_changes()

    def test_build_failure_does_not_stop_services(self):
        self.change_state(build_fail=True)
        self.start()
        self.assertNotEqual(0, self.finish())
        self.assertTrue(any(event[:2] == ["compose", "build"] for event in self.events()), self.output())
        self.assert_no_service_changes()

    def test_configuration_must_match_running_image(self):
        (self.root / "image.env").write_text(self.original_image_env.replace(OLD_SHA, NEW_SHA), encoding="utf-8")
        self.start()
        self.assertNotEqual(0, self.finish(), self.output())
        self.assertFalse(any(event[:2] == ["compose", "build"] for event in self.events()), self.output())
        self.assertFalse(any(event[0] == "compose" and event[1] in ("stop", "start", "up") for event in self.events()), self.output())

    def test_confirmation_completes_backup_and_switch(self):
        self.environment.update(APP_IMAGE_REPOSITORY="untrusted-repository", APP_IMAGE_TAG="untrusted-tag")
        self.start()
        self.wait_for_confirmation()
        self.assertEqual(0, self.finish(), self.output())
        state = self.state()
        self.assertTrue(state["app_running"])
        self.assertTrue(state["redis_running"])
        self.assertEqual(NEW_IMAGE, state["active_image"])
        self.assertEqual("new-api-fork", state["build_repository"])
        self.assertEqual(NEW_SHA, state["build_tag"])
        self.assertIn(f"APP_IMAGE_TAG={NEW_SHA}\n", (self.root / "image.env").read_text(encoding="utf-8"))
        backups = list((self.root / "backups").iterdir())
        self.assertEqual(1, len(backups))
        backup = backups[0]
        self.assertTrue((backup / "COMPLETE").is_file())
        self.assertEqual(self.original_image_env, (backup / "image.env").read_text(encoding="utf-8"))
        for line in (backup / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
            digest, filename = line.split(maxsplit=1)
            self.assertEqual(digest, hashlib.sha256((backup / Path(filename).name).read_bytes()).hexdigest())
        with tarfile.open(backup / "runtime-files.tar.gz") as archive:
            names = archive.getnames()
        self.assertIn("data/retained.txt", names)
        self.assertNotIn("images", names)
        actions = [(event[1], event[-1]) for event in self.events() if event[0] == "compose" and event[1] in ("build", "stop", "up", "start")]
        self.assertEqual([("build", "new-api"), ("stop", "new-api"), ("stop", "redis"),
                          ("up", "redis"), ("up", "new-api")], actions, self.output())

    def test_waiting_state_changes_are_rejected_before_stopping(self):
        for change in ("config", "container", "active_id", "built_id"):
            with self.subTest(change=change):
                self.start()
                self.wait_for_confirmation()
                if change == "config":
                    with (self.root / ".env").open("a", encoding="utf-8") as config:
                        config.write("CHANGED=1\n")
                else:
                    self.change_state(**{change: "changed-during-wait"})
                self.assertNotEqual(0, self.finish(), self.output())
                self.assert_no_service_changes()
                if change == "config":
                    (self.root / ".env").write_text("NEWAPI_PORT=4040\n", encoding="utf-8")
                else:
                    self.change_state(container="container-old", active_id=OLD_ID, built_id=NEW_ID)

    def test_backup_failures_restore_original_services(self):
        for failure in ("dump_fail", "redis_up_fail"):
            with self.subTest(failure=failure):
                self.change_state(**{failure: True})
                self.start()
                self.wait_for_confirmation()
                self.assertNotEqual(0, self.finish(), self.output())
                self.assertTrue(self.state()["app_running"], self.output())
                self.assertTrue(self.state()["redis_running"], self.output())
                self.assertEqual(OLD_IMAGE, self.state()["active_image"])
                self.assertEqual(self.original_image_env, (self.root / "image.env").read_text(encoding="utf-8"))
                self.assertIn(["compose", "start", "new-api"], self.events())
                self.assert_no_cache_cleanup()
                if failure == "redis_up_fail":
                    self.assertIn(["compose", "start", "redis"], self.events())
                self.change_state(**{failure: False})
                shutil.rmtree(self.root / "backups")

    def test_failures_after_switch_do_not_roll_back(self):
        for failure in ("app_up_fail", "health_fail"):
            with self.subTest(failure=failure):
                self.change_state(**{failure: True})
                self.start()
                self.wait_for_confirmation()
                self.assertNotEqual(0, self.finish(), self.output())
                self.assertIn(f"APP_IMAGE_TAG={NEW_SHA}\n", (self.root / "image.env").read_text(encoding="utf-8"))
                self.assertNotIn(["compose", "start", "new-api"], self.events())
                self.assert_no_cache_cleanup()
                self.assertTrue(any((directory / "COMPLETE").exists() for directory in (self.root / "backups").iterdir()))
                self.change_state(**{failure: False}, app_running=True, active_image=OLD_IMAGE, active_id=OLD_ID)
                (self.root / "image.env").write_text(self.original_image_env, encoding="utf-8")
                shutil.rmtree(self.root / "backups")

    def test_success_cleans_build_cache_after_health_check(self):
        for entrypoint in ("confirm", "ready"):
            for buildx_available in (True, False):
                for prune_fail in (False, True):
                    with self.subTest(entrypoint=entrypoint, buildx_available=buildx_available, prune_fail=prune_fail):
                        self.reset_deployment(entrypoint, buildx_available=buildx_available, prune_fail=prune_fail)
                        self.start(entrypoint)
                        self.assertEqual(0, self.finish(), self.output())
                        events = self.events()
                        prune = ["docker", "buildx" if buildx_available else "builder", "prune", "--all", "--force"]
                        self.assertEqual([prune], [event for event in events if event[0] == "docker" and "prune" in event], self.output())
                        health_index = next(index for index, event in enumerate(events) if event[0] == "curl")
                        self.assertLess(health_index, events.index(prune), self.output())
                        self.assertEqual(NEW_IMAGE, self.state()["active_image"])
                        self.assertTrue(self.state()["app_running"])
                        self.assertNotIn(["compose", "start", "new-api"], events)
                        warning = "警告：Docker 构建缓存清理失败，更新已成功；请检查上方错误并稍后手动清理。"
                        self.assertEqual(prune_fail, warning in self.output(), self.output())

    def test_unsuccessful_or_unnecessary_updates_do_not_clean_cache(self):
        failures = ("no_update", "build_fail", "dump_fail", "redis_up_fail", "app_up_fail", "health_fail")
        for entrypoint in ("confirm", "ready"):
            for failure in failures:
                with self.subTest(entrypoint=entrypoint, failure=failure):
                    changes = {"next_sha": OLD_SHA} if failure == "no_update" else {failure: True}
                    self.reset_deployment(entrypoint, **changes)
                    self.start(entrypoint)
                    result = self.finish()
                    if failure == "no_update":
                        self.assertEqual(0, result, self.output())
                    else:
                        self.assertNotEqual(0, result, self.output())
                    self.assert_no_cache_cleanup()


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--fake":
        sys.exit(fake_command(sys.argv[2], sys.argv[3:]))
    unittest.main()
