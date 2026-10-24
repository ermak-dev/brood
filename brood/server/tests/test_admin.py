"""Админка: без входа -- на страницу входа, вход по логину и паролю."""
import contextlib
import io

from django.contrib.auth import get_user_model
from django.test import SimpleTestCase, TestCase

from brood.common import cli
from brood.server.cli import main as server

PASSWORD = "brood-test-password"


class AdminLoginTests(TestCase):
    def setUp(self):
        get_user_model().objects.create_superuser("admin", "admin@example.com", PASSWORD)

    def test_anonymous_goes_to_login(self):
        self.assertRedirects(self.client.get("/admin/"), "/admin/login/?next=/admin/")

    def test_superuser_logs_in(self):
        r = self.client.post("/admin/login/?next=/admin/",
                             {"username": "admin", "password": PASSWORD})
        self.assertRedirects(r, "/admin/")
        self.assertContains(self.client.get("/admin/"), "Site administration")

    def test_wrong_password_refused(self):
        r = self.client.post("/admin/login/?next=/admin/",
                             {"username": "admin", "password": "wrong"})
        self.assertContains(r, "Please enter the correct username and password")
        self.assertRedirects(self.client.get("/admin/"), "/admin/login/?next=/admin/")


class ManageTests(SimpleTestCase):
    def test_server_words_go_to_manage_py(self):
        self.assertEqual(cli.resolve(["server", "migrate", "--plan"], cli.groups()),
                         ("brood.server.cli", ["migrate", "--plan"]))

    def test_check_runs_through_brood_server(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            server(["check"])
        self.assertIn("System check identified no issues", out.getvalue())
