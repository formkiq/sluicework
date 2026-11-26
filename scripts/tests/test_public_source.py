import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("public_check", Path(__file__).parents[1] / "check-public-source.py")
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class PublicSourceTests(unittest.TestCase):
    def test_private_infrastructure_references_are_literal_case_insensitive_and_redacted(self):
        with tempfile.TemporaryDirectory() as temp:
            file = Path(temp) / "references.txt"
            file.write_text("# protected local references\nBuild-Node.example.internal\nteam/private-project\n")
            file.chmod(0o600)
            references = checker.read_private_references(file)
            value = b"https://BUILD-NODE.example.internal/api"
            result = checker.findings("guide.md", value, private_references=references)
            self.assertTrue(result)
            self.assertNotIn("example.internal", str(result))
            self.assertTrue(checker.findings("team/private-project.md", b"", private_references=references))
            self.assertEqual(checker.findings("guide.md", b"Build-NodeXexampleXinternal", private_references=references), [])
            file.chmod(0o644)
            with self.assertRaises(ValueError):
                checker.read_private_references(file)
            file.chmod(0o600)
            link = Path(temp) / "link"
            link.symlink_to(file)
            with self.assertRaises(OSError):
                checker.read_private_references(link)
            pipe = Path(temp) / "pipe"
            os.mkfifo(pipe, 0o600)
            with self.assertRaises(ValueError):
                checker.read_private_references(pipe)

    def test_rejects_identifiers_and_credentials_without_exposing_values(self):
        samples = [b"ghp_" + b"A" * 30, b"-----BEGIN " + b"PRIVATE KEY-----",
                   b"9876" + b"54321098", b"subnet-" + b"ab" * 8,
                   b"/Users/" + b"private-person/project"]
        for value in samples:
            result = checker.findings("example.txt", value)
            self.assertTrue(result)
            self.assertNotIn(value.decode(), str(result))

    def test_permits_documented_synthetic_examples(self):
        self.assertEqual(checker.findings(".env.example", b"SLUICEWORK_CONFIG=/absolute/path/server.json"), [])
        self.assertEqual(checker.findings("example.json", b'"account":"123456789012", "ami":"ami-00000000000000001"'), [])
        self.assertEqual(checker.findings("example.sh", b"/home/runner /Users/fixture"), [])
        self.assertEqual(checker.findings("example.sh", b'$scratch/home/.local/bin'), [])
        self.assertEqual(checker.findings("example.json", b'"id":"00000000-0000-4000-8000-000000000001"'), [])

    def test_private_paths_symlinks_and_nested_repositories_fail(self):
        for name in [".env.local", "local/settings.json", "config/private/settings.json", "worker.json", "key.pem", "state.tfstate.backup", ".github/sluicework.json"]:
            self.assertTrue(checker.findings(name, b"{}"), name)
        self.assertTrue(checker.findings("reference", b"", "120000"))
        self.assertTrue(checker.findings("nested", b"", "160000"))

    def test_queue_examples_must_use_synthetic_app_ids_and_organization(self):
        config = {"backendId": "example", "appId": 100001, "installationId": 100002,
                  "repositories": [{"name": "example-org/application"}]}
        self.assertEqual(checker.findings("example.json", json.dumps(config).encode()), [])
        config["appId"] = 9001
        self.assertTrue(checker.findings("example.json", json.dumps(config).encode()))
        config["appId"] = 100001
        config["repositories"] = [{"name": "customer-org/application"}]
        self.assertTrue(checker.findings("example.json", json.dumps(config).encode()))

    def test_history_finds_removed_private_files_and_staged_checks_ignore_unstaged_cleanup(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            def git(*args):
                return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.DEVNULL)
            git("init", "-b", "main")
            git("config", "user.name", "Fixture")
            git("config", "user.email", "fixture@example.com")
            private = root / ".env.local"
            private.write_text("CONFIG=/private/runtime/server.json\n")
            git("add", ".env.local")
            git("commit", "-qm", "Add fixture")
            private.unlink()
            (root / "README.md").write_text("Synthetic example\n")
            git("add", "-A")
            git("commit", "-qm", "Remove fixture")
            self.assertEqual(checker.check(root), [])
            self.assertTrue(checker.check(root, history=True))
            source = root / "example.txt"
            source.write_bytes(b"ghp_" + b"A" * 30)
            git("add", "example.txt")
            source.write_text("Clean working copy\n")
            self.assertEqual(checker.check(root), [])
            self.assertTrue(checker.check(root, staged=True))


if __name__ == "__main__":
    unittest.main()
