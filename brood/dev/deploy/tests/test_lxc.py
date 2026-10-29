"""Подготовка LXC ограничена собственным config и образом, без изменения пулов."""
import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from brood.dev.deploy import infrastructure, lxc


IMAGE = f"{infrastructure.REGISTRY}/ateworker-lxc@sha256:" + "d" * 64
CRD = {"spec": {"versions": [{"name": "v1", "served": True, "schema": {
    "openAPIV3Schema": {"properties": {"spec": {"properties": {
        "sandboxClasses": {"items": {"properties": {"name": {
            "enum": ["gvisor", "microvm", "lxc"]}}}}}}}}}}]}}
NAMESPACE = {"metadata": {"labels": {infrastructure.OWNER[0]: infrastructure.OWNER[1]}}}


# FEATURE: Явная подготовка установленного LXC без миграции рабочих пулов (#74).
# SPEC: Сначала владение и CRD, затем digest image и свой явно адресуемый config;
# чужой/изменённый config не переписывается, worker pools и акторы не меняются.
class LXCTests(unittest.TestCase):
    def test_digest_is_only_from_this_installation_lxc_repository(self):
        self.assertEqual(lxc.image_digest(json.dumps([IMAGE])), IMAGE)
        for digests in ["invalid", "{}", "[]", json.dumps([IMAGE, IMAGE]),
                        json.dumps(["other/ateworker-lxc@sha256:" + "d" * 64]),
                        json.dumps([IMAGE.replace("@sha256:", ":latest-")])]:
            with self.subTest(digests=digests), self.assertRaisesRegex(RuntimeError, "LXC worker image"):
                lxc.image_digest(digests)

    def test_installed_api_must_explicitly_serve_the_lxc_class(self):
        lxc.require_api(CRD)
        old = copy.deepcopy(CRD)
        old["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"][
            "properties"]["sandboxClasses"]["items"]["properties"]["name"]["enum"] = ["gvisor", "microvm"]
        unserved = copy.deepcopy(CRD)
        unserved["spec"]["versions"][0]["served"] = False
        for crd in [{}, [], old, unserved]:
            with self.subTest(crd=crd), self.assertRaisesRegex(RuntimeError, "installed Substrate.*LXC"):
                lxc.require_api(crd)

    def test_configuration_never_sets_or_replaces_a_global_class_default(self):
        desired = lxc.configuration({"items": []})
        self.assertNotIn("sandboxconfig.ate.dev/is-class-default", desired["metadata"].get("annotations", {}))
        other = copy.deepcopy(desired)
        other["metadata"].update(name="other-lxc", uid="other", resourceVersion="3",
                                 annotations={"sandboxconfig.ate.dev/is-class-default": "true"})
        self.assertEqual(lxc.configuration({"items": [other]}), desired)

    def test_configuration_matches_verified_fork_and_is_class_scoped(self):
        desired = lxc.configuration({"items": []})
        self.assertEqual(desired["kind"], "SandboxConfig")
        self.assertEqual(desired["metadata"]["name"], "lxc-image")
        self.assertEqual(desired["metadata"]["labels"], {infrastructure.OWNER[0]: infrastructure.OWNER[1]})
        self.assertNotIn("sandboxconfig.ate.dev/is-class-default", desired["metadata"].get("annotations", {}))
        self.assertEqual(desired["spec"], {"sandboxClass": "lxc", "defaultVersion": "image",
                         "versions": [{"name": "image", "state": "Enabled",
                                       "assets": {"amd64": {}, "arm64": {}}}]})
        owned = copy.deepcopy(desired)
        owned["metadata"].update(uid="own-config", resourceVersion="7")
        self.assertIsNone(lxc.configuration({"items": [owned]}))
        for mutate in [lambda item: item["metadata"].pop("labels"),
                       lambda item: item["metadata"].update(deletionTimestamp="now"),
                       lambda item: item["metadata"].pop("uid"),
                       lambda item: item["spec"].update(defaultVersion="other"),
                       lambda item: item["metadata"].update(annotations={
                           "sandboxconfig.ate.dev/is-class-default": "true"}),
                       lambda item: item["metadata"].update(labels={infrastructure.OWNER[0]: "foreign"})]:
            existing = copy.deepcopy(owned)
            mutate(existing)
            with self.subTest(existing=existing), self.assertRaises(RuntimeError):
                lxc.configuration({"items": [existing]})

    # BUG: #84: API добавляет state=Enabled и ломает точное сравнение своего config.
    # HYPOTHESIS: явный native default сохранит строгую проверку spec при повторе.
    # SOLUTION: desired явно содержит state=Enabled, без нормализации чужого spec.
    # RESULT: FIXED — повтор не пишет config; Disabled и неизвестные state отвергаются.
    # STATUS: FIXED
    def test_prepare_reuses_api_defaulted_owned_configuration_without_writes(self):
        owned = lxc.configuration({"items": []})
        owned["metadata"].update(uid="own-config", resourceVersion="7")
        owned["spec"]["versions"][0]["state"] = "Enabled"
        before = copy.deepcopy(owned)
        cluster = self.cluster(configs={"items": [owned]})
        build = Mock(return_value=IMAGE)
        try:
            result = lxc.Deploy(cluster, build=build).prepare()
        except RuntimeError as error:
            self.fail(f"API-defaulted owned configuration must be reusable: {error}")
        self.assertEqual(result, IMAGE)
        self.assertEqual(owned, before)
        self.assertFalse(cluster.created)
        build.assert_called_once_with(cluster)
        self.assertNotIn("create", str(cluster.shell.mock_calls))
        self.assertNotIn("delete", str(cluster.shell.mock_calls))
        self.assertNotIn("scale", str(cluster.shell.mock_calls))
        cluster.shell.run.assert_not_called()

    def test_configuration_refuses_disabled_or_unknown_version_state(self):
        for state in ("Disabled", "unknown", None):
            with self.subTest(state=state):
                owned = lxc.configuration({"items": []})
                owned["metadata"].update(uid="own-config", resourceVersion="7")
                owned["spec"]["versions"][0]["state"] = state
                with self.assertRaisesRegex(RuntimeError, "identity or spec"):
                    lxc.configuration({"items": [owned]})

    def cluster(self, crd=CRD, configs=None):
        shell = Mock(env={"PATH": "/bin"})
        responses = iter([(0, json.dumps(NAMESPACE), ""),
                          (0, json.dumps(crd), ""),
                          (0, json.dumps(configs or {"items": []}), ""),
                          (0, "created", "")])
        created = []

        def output(*args):
            if "create" in args:
                with open(args[args.index("-f") + 1], encoding="utf-8") as stream:
                    created.append(json.load(stream))
            return next(responses)

        shell.output.side_effect = output
        return SimpleNamespace(shell=shell, source="/source", check=Mock(), created=created)

    def test_prepare_creates_only_config_after_ownership_and_image_validation(self):
        cluster = self.cluster()
        build = Mock(return_value=IMAGE)
        self.assertEqual(lxc.Deploy(cluster, build=build).prepare(), IMAGE)
        self.assertEqual(cluster.check.call_count, 2)
        build.assert_called_once_with(cluster)
        writes = [call.args for call in cluster.shell.output.call_args_list if "create" in call.args]
        self.assertEqual(len(writes), 1)
        self.assertEqual([manifest["kind"] for manifest in cluster.created], ["SandboxConfig"])
        self.assertEqual(cluster.created[0]["spec"]["sandboxClass"], "lxc")
        self.assertNotIn("scale", str(cluster.shell.mock_calls))
        self.assertNotIn("delete", str(cluster.shell.mock_calls))
        cluster.shell.run.assert_not_called()

    def test_old_api_or_invalid_image_cannot_change_native_resources(self):
        cluster = self.cluster(crd={})
        build = Mock(return_value=IMAGE)
        with self.assertRaisesRegex(RuntimeError, "installed Substrate.*LXC"):
            lxc.Deploy(cluster, build=build).prepare()
        build.assert_not_called()
        self.assertFalse(any("create" in call.args for call in cluster.shell.output.call_args_list))
        cluster = self.cluster()
        with self.assertRaisesRegex(RuntimeError, "LXC worker image"):
            lxc.Deploy(cluster, build=Mock(return_value="worker:latest")).prepare()
        self.assertFalse(any("create" in call.args for call in cluster.shell.output.call_args_list))

    def test_ownership_is_rechecked_after_build_before_native_write(self):
        cluster = self.cluster()
        cluster.check.side_effect = [None, RuntimeError("container ownership changed during build")]
        with self.assertRaisesRegex(RuntimeError, "ownership changed"):
            lxc.Deploy(cluster, build=Mock(return_value=IMAGE)).prepare()
        self.assertEqual(cluster.created, [])

    def test_cluster_ownership_failure_precedes_even_reads_or_build(self):
        cluster = self.cluster()
        cluster.check.side_effect = RuntimeError("container ownership changed")
        build = Mock()
        with self.assertRaisesRegex(RuntimeError, "ownership"):
            lxc.Deploy(cluster, build=build).prepare()
        cluster.shell.output.assert_not_called()
        build.assert_not_called()
