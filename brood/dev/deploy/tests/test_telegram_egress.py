import unittest
from unittest.mock import Mock, call

from brood.dev.deploy import go_server


def answer(address, port=443):
    import socket
    return (socket.AF_INET6 if ":" in address else socket.AF_INET, socket.SOCK_STREAM, 6,
            "", (address, port, 0, 0))


class TelegramEgressTests(unittest.TestCase):
    """Egress к Bot API (#286): резолв при выкладке, никаких правил для
    loopback-имени установки, отказ называет настройку."""

    def test_remote_origin_resolves_to_https_rules(self):
        resolver = Mock(side_effect=lambda host, port, **kw: {
            "api.telegram.org": [answer("149.154.167.220"), answer("2001:67c:4e8:f004::9")],
        }[host])
        self.assertEqual(go_server.telegram_egress("https://api.telegram.org", resolver), [
            {"to": [{"ipBlock": {"cidr": "149.154.167.220/32"}}],
             "ports": [{"port": 443, "protocol": "TCP"}]},
            {"to": [{"ipBlock": {"cidr": "2001:67c:4e8:f004::9/128"}}],
             "ports": [{"port": 443, "protocol": "TCP"}]},
        ])
        self.assertEqual(resolver.call_args_list, [call("api.telegram.org", 443, type=1)])

    def test_loopback_origin_has_no_rules_and_no_resolution(self):
        # Локальный Bot API сервер установки: egress не нужен, резолв не зовётся.
        resolver = Mock(side_effect=AssertionError("unexpected DNS"))
        for raw in ("http://127.0.0.1:8081", "http://localhost:8081"):
            with self.subTest(raw=raw):
                self.assertEqual(go_server.telegram_egress(raw, resolver), [])
        resolver.assert_not_called()

    def test_failed_resolution_refuses_naming_the_setting(self):
        import socket
        for resolver in (Mock(return_value=[]),
                         Mock(side_effect=socket.gaierror("api.telegram.org"))):
            with self.assertRaises(RuntimeError) as raised:
                go_server.telegram_egress("https://api.telegram.org", resolver)
            self.assertIn("BROOD_TELEGRAM_API", str(raised.exception))
            self.assertNotIn("telegram.org", str(raised.exception))

    def test_invalid_origin_refuses_naming_the_setting(self):
        for raw in ("ftp://api.telegram.org", "https://api.telegram.org/bot",
                    "https://user@api.telegram.org", "not a url",
                    "http://botapi.lan.example:8081"):
            with self.subTest(raw=raw[:40]):
                with self.assertRaises(RuntimeError) as raised:
                    go_server.telegram_egress(raw, Mock(side_effect=AssertionError))
                self.assertIn("BROOD_TELEGRAM_API", str(raised.exception))

    def test_none_origin_means_public_default(self):
        # Нет настройки — публичный Bot API по умолчанию сервера (#281).
        resolver = Mock(return_value=[answer("149.154.167.220")])
        self.assertEqual(go_server.telegram_egress(None, resolver), [
            {"to": [{"ipBlock": {"cidr": "149.154.167.220/32"}}],
             "ports": [{"port": 443, "protocol": "TCP"}]},
        ])
        self.assertEqual(resolver.call_args_list, [call("api.telegram.org", 443, type=1)])
