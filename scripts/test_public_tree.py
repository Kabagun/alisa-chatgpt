import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("public_tree", Path(__file__).with_name("check-public-tree.py"))
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class PublicTreeTests(unittest.TestCase):
    def test_credential_diagnostic_is_redacted(self):
        secret = "ghp_" + "a" * 40
        result = checker.findings("code.py", ("value = '" + secret + "'").encode())
        self.assertEqual(result, [{"path": "code.py", "line": 1, "category": "github_token"}])
        self.assertNotIn(secret, str(result))

    def test_runtime_file_is_rejected_even_with_empty_content(self):
        self.assertIn("forbidden_artifact", [x["category"] for x in checker.findings("station/session.json", b"{}")])

    def test_synthetic_project_and_email_are_allowed(self):
        fixture = ('g-p-' + '0' * 32 + ' ignored@example.invalid').encode()
        self.assertEqual(checker.findings("fixture_test.go", fixture), [])

    def test_realistic_non_synthetic_project_identifier_is_rejected(self):
        fixture = ('g-p-' + '01234567' * 4).encode()
        self.assertEqual(checker.findings("config.json", fixture)[0]["category"], "non_synthetic_project_id")

    def test_private_ip_requires_fixture_context(self):
        fixture = ('192.168.' + '99.5').encode()
        self.assertEqual(checker.findings("fixture_test.go", fixture), [])
        self.assertEqual(checker.findings("config.json", fixture)[0]["category"], "private_network_literal")

    def test_security_blocklist_and_negative_url_fixtures_are_allowed(self):
        self.assertEqual(checker.findings("policy.go", b'"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"'), [])
        self.assertEqual(checker.findings("url_test.go", b'https://user@fixture.chatgpt.site https://user:fixture@storage.example.com'), [])


if __name__ == "__main__":
    unittest.main()
