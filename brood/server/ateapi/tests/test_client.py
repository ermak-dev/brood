"""Плагин токена и композиция учётных данных gRPC: проверяются по-настоящему,
без канала -- оба раза баг всплывал на стенде, потому что проверки подменяли
клиент целиком."""
import unittest

import grpc

from brood.server.ateapi.client import BearerToken


def invoke(plugin):
    """Вызвать плагин как его зовет gRPC: (context, callback); вернёт то, что
    плагин отдал в callback."""
    done = {}

    def callback(metadata, error):
        done["metadata"], done["error"] = metadata, error

    plugin(object(), callback)  # context плагину не нужен
    return done


class BearerTokenTests(unittest.TestCase):
    def test_reads_token_per_call(self):
        """Токен читается с диска при каждом вызове: ротация kubelet'ом видна
        без пересборки канала."""
        with open("/tmp/brood-token-test", "w", encoding="utf-8") as f:
            f.write("first\n")
        try:
            plugin = BearerToken("/tmp/brood-token-test")
            wrapped = grpc.metadata_call_credentials(plugin)
            # Композиция -- то, что падало AttributeError'ом на голом классе.
            grpc.composite_channel_credentials(
                grpc.ssl_channel_credentials(root_certificates=b""), wrapped)
            result = invoke(plugin)
            self.assertEqual(result["metadata"], (("authorization", "Bearer first"),))
            self.assertIsNone(result["error"])
            with open("/tmp/brood-token-test", "w", encoding="utf-8") as f:
                f.write("second")
            self.assertEqual(invoke(plugin)["metadata"],
                             (("authorization", "Bearer second"),))
        finally:
            import os

            os.remove("/tmp/brood-token-test")


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
