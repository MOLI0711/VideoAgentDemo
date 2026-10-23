import contextlib
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("local_setup", Path(__file__).with_name("local.py"))
local = importlib.util.module_from_spec(spec)
spec.loader.exec_module(local)


class LocalSetupTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / "configs").mkdir()
        (self.root / "configs/container.example.yaml").write_text(
            "origin: http://127.0.0.1:5173\npublic_s3: http://127.0.0.1:18333\n", encoding="utf-8"
        )

    def initialize(self, web=5180, s3=18433):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            local.initialize(self.root, web, s3)
        return output.getvalue()

    def test_generated_keys_match_private_s3_and_never_print(self):
        output = self.initialize()
        directory = self.root / ".runtime/container"
        values = local.read_env(directory / "env")
        identity = local.json.loads((directory / "s3.json").read_text())["identities"][0]["credentials"][0]
        self.assertEqual(identity["accessKey"], values["VA_S3_ACCESS_KEY"])
        self.assertEqual(identity["secretKey"], values["VA_S3_SECRET_KEY"])
        self.assertIn("@tcp(mysql:3306)/video_agent?parseTime=true&loc=UTC", values["VA_MYSQL_DSN"])
        self.assertEqual(values["VA_MODEL_API_KEY"], "")
        self.assertEqual(values["VA_PUBLIC_ORIGIN"], "http://127.0.0.1:5180")
        self.assertIn("http://127.0.0.1:18433", (directory / "config.yaml").read_text())
        self.assertNotIn(values["MYSQL_ROOT_PASSWORD"], output)
        if os.name != "nt":
            self.assertEqual((directory / "env").stat().st_mode & 0o777, 0o600)
            self.assertEqual((directory / "config.yaml").stat().st_mode & 0o777, 0o644)

    def test_repeated_initialization_preserves_existing_keys_and_settings(self):
        self.initialize()
        directory = self.root / ".runtime/container"
        (directory / "config.yaml").write_text("custom developer configuration\n")
        before = {p.name: p.read_bytes() for p in directory.iterdir()}
        self.initialize(5999, 19999)
        self.assertEqual(before, {p.name: p.read_bytes() for p in directory.iterdir()})

    def test_incomplete_configuration_is_never_rekeyed(self):
        self.initialize()
        directory = self.root / ".runtime/container"
        before = (directory / "env").read_bytes()
        (directory / "s3.json").unlink()
        with self.assertRaises(ValueError):
            self.initialize()
        self.assertEqual(before, (directory / "env").read_bytes())

    def test_ports_are_validated_before_creating_files(self):
        for pair in [(80, 18333), (5173, 5173), (70000, 18333)]:
            with self.assertRaises(ValueError):
                self.initialize(*pair)
        self.assertFalse((self.root / ".runtime").exists())

    def test_provider_output_is_redacted(self):
        values = {"VA_MODEL_API_KEY": "private-model-value", "MYSQL_ROOT_PASSWORD": "db-secret", "VA_MYSQL_DSN": "root:db-secret@tcp(mysql:3306)/db", "VA_PUBLIC_ORIGIN": "http://127.0.0.1:5180"}
        message = " ".join(values.values())
        result = local.redact(message, values)
        self.assertNotIn("private-model-value", result)
        self.assertNotIn("db-secret", result)
        self.assertNotIn("root:", result)
        self.assertIn("http://127.0.0.1:5180", result)

    @unittest.skipIf(os.name == "nt", "POSIX permissions")
    def test_private_umask_does_not_break_non_root_container(self):
        previous = os.umask(0o077)
        try:
            self.initialize()
        finally:
            os.umask(previous)
        directory = self.root / ".runtime/container"
        self.assertEqual((directory / "config.yaml").stat().st_mode & 0o777, 0o644)
        self.assertEqual((directory / "env").stat().st_mode & 0o777, 0o600)

    def test_private_file_wins_over_unrelated_shell_credentials(self):
        values = {"MYSQL_ROOT_PASSWORD": "workspace-password", "VA_WEB_PORT": "5180"}
        class Child:
            stdout = []
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def wait(self): return 0
        with patch.dict(os.environ, {"MYSQL_ROOT_PASSWORD": "other-project-password"}):
            with patch.object(local.subprocess, "Popen", return_value=Child()) as create:
                local.run(["unused-compose-test"], values)
        self.assertEqual(create.call_args.kwargs["env"]["MYSQL_ROOT_PASSWORD"], "workspace-password")
        self.assertEqual(create.call_args.kwargs["env"]["VA_WEB_PORT"], "5180")

    def test_env_parser_never_evaluates_shell(self):
        path = self.root / "env"
        path.write_text("# comment\nVA_MYSQL_DSN=root:p@tcp(mysql:3306)/db?parseTime=true&loc=UTC\nVA_MODEL_API_KEY=$(do-not-run)\n")
        values = local.read_env(path)
        self.assertEqual(values["VA_MODEL_API_KEY"], "$(do-not-run)")
        self.assertTrue(values["VA_MYSQL_DSN"].endswith("&loc=UTC"))


if __name__ == "__main__":
    unittest.main()
