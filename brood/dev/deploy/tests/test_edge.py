"""Проверка входа после выкладки: сертификат сверяется и тогда, когда ответ
закрывает соединение -- так отвечает старый под edge, пока nginx плавно
останавливается во время раскатки."""
import unittest
from unittest import mock

from brood.dev.deploy import server

# PEM_cert_to_DER_cert лишь снимает рамку и декодирует base64.
CERT = "-----BEGIN CERTIFICATE-----\nAAEC\n-----END CERTIFICATE-----\n"
SERVED = b"\x00\x01\x02"


class Connection:
    """HTTPSConnection, чей ответ несёт Connection: close: http.client отдаёт
    сокет ответу и обнуляет conn.sock в getresponse()."""

    served = SERVED

    def __init__(self, *_args, **_kwargs):
        self.sock = mock.Mock(getpeercert=lambda binary_form: self.served)

    def request(self, *_args, **_kwargs):
        pass

    def getresponse(self):
        self.sock = None
        return mock.Mock(status=200)

    def close(self):
        pass


class EdgeCheckTests(unittest.TestCase):
    def deploy(self):
        return server.Deploy(mock.Mock(), get=lambda _: "", clock=lambda: 0, sleep=lambda _: None)

    def test_certificate_read_before_a_closing_response(self):
        with mock.patch.object(server.http.client, "HTTPSConnection", Connection):
            self.deploy()._answers(443, "brood", CERT, "GET", "/", {}, 200)

    def test_another_certificate_is_refused(self):
        class Foreign(Connection):
            served = b"\xff"

        with mock.patch.object(server.http.client, "HTTPSConnection", Foreign):
            with self.assertRaisesRegex(RuntimeError, "another certificate"):
                self.deploy()._answers(443, "brood", CERT, "GET", "/", {}, 200)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
