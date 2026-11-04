import unittest

from test_database_matrix import Suite, configure_environment, evaluate_events, validate_dsn


class DatabaseMatrixContractTests(unittest.TestCase):
    def test_external_runs_require_all_disposable_databases(self):
        source = {
            "TEST_MYSQL_DSN": "root@tcp(127.0.0.1:3306)/new_api_image_test_ci?parseTime=True",
            "TEST_MYSQL_LOG_DSN": "root@tcp(127.0.0.1:3306)/new_api_image_test_ci_logs",
            "IMAGE_WORKER_TEST_MYSQL_DSN": "root@tcp(127.0.0.1:3306)/new_api_image_worker_test_ci",
        }
        for missing in source:
            with self.subTest(missing=missing), self.assertRaises(ValueError):
                configure_environment("mysql", {key: value for key, value in source.items() if key != missing})
        env, endpoints = configure_environment("mysql", source)
        self.assertEqual(env["ASYNC_IMAGE_TEST_MYSQL_DSN"], source["TEST_MYSQL_DSN"])
        self.assertEqual(env["RETRY_POLICY_TEST_DSN"], source["TEST_MYSQL_DSN"])
        self.assertEqual(len(endpoints), 3)
        source["TEST_MYSQL_LOG_DSN"] = source["TEST_MYSQL_DSN"]
        with self.assertRaises(ValueError):
            configure_environment("mysql", source)

    def test_postgres_urls_and_isolation(self):
        source = {
            "TEST_POSTGRES_DSN": "postgres://postgres@127.0.0.1:5432/new_api_image_test_ci?sslmode=disable",
            "TEST_POSTGRES_LOG_DSN": "postgres://postgres@127.0.0.1:5432/new_api_image_test_ci_logs?sslmode=disable",
            "IMAGE_WORKER_TEST_PG_DSN": "postgres://postgres@127.0.0.1:5432/new_api_image_worker_test_ci?sslmode=disable",
            "TEST_MYSQL_DSN": "must not select MySQL in a PostgreSQL run",
        }
        env, _ = configure_environment("postgres", source)
        self.assertNotIn("TEST_MYSQL_DSN", env)
        self.assertEqual(env["ASYNC_IMAGE_TEST_PG_DSN"], source["TEST_POSTGRES_DSN"])
        self.assertEqual(env["RETRY_POLICY_TEST_DIALECT"], "postgres")

    def test_unsafe_or_ambiguous_connection_targets_are_rejected(self):
        for dialect, dsn in (
            ("mysql", "root@tcp(192.0.2.1:3306)/new_api_image_test_ci"),
            ("mysql", "root@tcp(127.0.0.1:3306)/production"),
            ("mysql", "root@unix(/tmp/mysql.sock)/new_api_image_test_ci"),
            ("mysql", "root@tcp(127.0.0.1:3306)/new_api_image_test_ci?allowAllFiles=true"),
            ("postgres", "host=127.0.0.1 dbname=new_api_image_test_ci"),
            ("postgres", "postgres://postgres@localhost/new_api_image_test_ci"),
            ("postgres", "postgres://postgres@127.0.0.1/production"),
            ("postgres", "postgres://postgres@127.0.0.1/new_api_image_test_ci?host=192.0.2.1"),
            ("postgres", "postgres://postgres@127.0.0.1/new_api_image_test_ci?dbname=production"),
            ("postgres", "postgres://postgres@127.0.0.1/new_api_image_test_ci?sslmode=disable&sslmode=require"),
        ):
            with self.subTest(dialect=dialect, dsn=dsn), self.assertRaises(ValueError):
                validate_dsn(dialect, dsn, "TEST_DSN")

    def test_sqlite_cannot_inherit_an_external_fixture(self):
        source = {"TEST_MYSQL_DSN": "unsafe", "ASYNC_IMAGE_TEST_PG_DSN": "unsafe",
                  "IMAGE_WORKER_TEST_REDIS_ADDR": "unsafe", "SQL_DSN": "unsafe", "PATH": "kept"}
        env, endpoints = configure_environment("sqlite", source)
        self.assertEqual(endpoints, [])
        self.assertEqual(env["PATH"], "kept")
        for key in source.keys() - {"PATH"}:
            self.assertNotIn(key, env)

    def test_address_aliases_cannot_hide_a_shared_log_database(self):
        source = {
            "TEST_POSTGRES_DSN": "postgres://postgres@[::1]:5432/new_api_image_test_ci",
            "TEST_POSTGRES_LOG_DSN": "postgres://postgres@[0:0:0:0:0:0:0:1]:5432/new_api_image_test_ci",
            "IMAGE_WORKER_TEST_PG_DSN": "postgres://postgres@[::1]:5432/new_api_image_worker_test_ci",
        }
        with self.assertRaises(ValueError):
            configure_environment("postgres", source)
        endpoint = validate_dsn("postgres", source["TEST_POSTGRES_LOG_DSN"], "TEST_POSTGRES_LOG_DSN")
        self.assertEqual(endpoint["host"], "::1")

    def test_success_requires_execution_and_pass_of_every_required_test(self):
        suite = Suite("contract", "./model", ("TestRequired",))
        events = [{"Action": "run", "Test": "TestRequired"},
                  {"Action": "pass", "Test": "TestRequired"}, {"Action": "pass"}]
        self.assertEqual(evaluate_events(suite, events, 0)["errors"], [])
        for invalid in (
            [{"Action": "pass"}],
            events[1:],
            events[:-1],
            events + [{"Action": "skip", "Test": "TestRequired/essential"}],
            events + [{"Action": "fail", "Test": "TestRequired/essential"}],
        ):
            with self.subTest(events=invalid):
                self.assertTrue(evaluate_events(suite, invalid, 0)["errors"])
        self.assertTrue(evaluate_events(suite, events, 1)["errors"])

    def test_selected_dialect_requires_its_concurrency_cases_and_real_version(self):
        suite = Suite("channels", "./model", ("TestTaskVideoModelBindingWrites",), "postgres")
        events = [event for name in suite.required() for event in (
            {"Action": "run", "Test": name}, {"Action": "pass", "Test": name})]
        events.append({"Action": "pass"})
        self.assertTrue(evaluate_events(suite, events, 0)["errors"])
        version = {"Action": "output", "Test": "TestTaskVideoModelBindingWrites/postgres",
                   "Output": "postgres version: PostgreSQL 16.15\n"}
        events.append(version)
        self.assertEqual(evaluate_events(suite, events, 0)["errors"], [])
        missing = [event for event in events if event.get("Test") != "TestTaskVideoModelBindingWrites/postgres/batch_against_batch"]
        self.assertTrue(evaluate_events(suite, missing, 0)["errors"])
        wrong = dict(version, Test="TestTaskVideoModelBindingWrites/sqlite")
        self.assertTrue(evaluate_events(suite, events[:-1] + [wrong], 0)["errors"])


if __name__ == "__main__":
    unittest.main()
