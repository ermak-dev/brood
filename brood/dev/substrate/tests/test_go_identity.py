"""Router допускает серверную identity без удаления identity старого клиента."""
import unittest

from brood.dev.substrate import local


class GoRouterIdentityTests(unittest.TestCase):
    def test_router_allows_only_explicit_server_and_legacy_client_identities(self):
        flag = next(value for value in local.DEPLOY if value.startswith("--ingress-allowed-spiffe-ids="))
        values = flag.split("=", 1)[1].split(",")
        self.assertCountEqual(values, ["spiffe://cluster.local/ns/ate-system/sa/ate-client",
                                       "spiffe://cluster.local/ns/brood/sa/django"])
