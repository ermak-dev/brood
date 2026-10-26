import json
import unittest

from brood.dev.workbench import manifest


IMAGE = "ghcr.io/example/workbench@sha256:" + "a" * 64
PURPOSE = {"brood.dev/purpose": "development"}
CAPABILITIES = """
CHOWN DAC_OVERRIDE DAC_READ_SEARCH FOWNER FSETID KILL SETGID SETUID SETPCAP
LINUX_IMMUTABLE NET_BIND_SERVICE NET_BROADCAST NET_ADMIN NET_RAW IPC_LOCK IPC_OWNER
SYS_MODULE SYS_RAWIO SYS_CHROOT SYS_PTRACE SYS_PACCT SYS_ADMIN SYS_BOOT SYS_NICE
SYS_RESOURCE SYS_TIME SYS_TTY_CONFIG MKNOD LEASE AUDIT_WRITE AUDIT_CONTROL SETFCAP
MAC_OVERRIDE MAC_ADMIN SYSLOG WAKE_ALARM BLOCK_SUSPEND AUDIT_READ PERFMON BPF
CHECKPOINT_RESTORE
""".split()


class ManifestTests(unittest.TestCase):
    def test_template_contract(self):
        result = manifest.actor_template(IMAGE, "builds")
        self.assertEqual(set(result), {
            "metadata", "workerSelector", "resources", "containers", "volumes",
            "snapshotConfig"})
        self.assertEqual(result["metadata"], {
            "atespace": "builds", "name": "brood-build-test-deploy"})
        self.assertEqual(result["workerSelector"], {"matchLabels": PURPOSE})
        self.assertEqual(result["resources"], {"limits": {
            "cpuMillicores": "4000", "memoryBytes": "8589934592"}})
        self.assertEqual(result["snapshotConfig"], {
            "preferredFidelity": "SNAPSHOT_FIDELITY_MEMORY",
            "goldenSnapshotConfig": {"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}})
        self.assertEqual(result["volumes"], [{"name": "workspace", "durableDir": {}}])
        self.assertEqual(len(result["containers"]), 1)
        container = result["containers"][0]
        self.assertEqual(set(container), {
            "name", "image", "command", "args", "env", "volumeMounts", "securityContext"})
        self.assertEqual(container["name"], "main")
        self.assertEqual(container["image"], IMAGE)
        self.assertEqual(container["command"], [
            "/usr/bin/tini", "--", "/opt/seeds/brood/bin/brood", "workbench", "start"])
        self.assertEqual(container["args"], ["--", "bash", ".ci/workbench.sh"])
        self.assertEqual(container["volumeMounts"], [
            {"name": "workspace", "mountPath": "/workspace"}])
        self.assertEqual(container["env"], [
            {"name": name, "value": value} for name, value in {
                "HOME": "/workspace/home", "XDG_DATA_HOME": "/workspace/data",
                "SUBSTRATE_SOURCE": "/workspace/substrate", "TMPDIR": "/workspace/tmp",
                "DOCKER_HOST": "unix:///var/run/docker.sock", "PIP_FIND_LINKS": "/opt/wheels",
                "PIP_NO_INDEX": "1"}.items()])
        self.assertEqual(container["securityContext"], {
            "capabilities": {"add": CAPABILITIES}})
        self.assertEqual(len(set(CAPABILITIES)), 41)
        serialized = json.dumps(result)
        for forbidden in ("hostPath", "privileged", "kubeconfig", "goldenEgressPolicy"):
            self.assertNotIn(forbidden, serialized)

    def test_pool_contract(self):
        self.assertEqual(manifest.worker_pool(IMAGE), {
            "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
            "metadata": {"name": "brood-development", "namespace": "brood", "labels": PURPOSE},
            "spec": {"replicas": 1, "workerImage": IMAGE,
                     "sandboxClasses": [{"name": "microvm", "configRef": {"name": "microvm"}}],
                     "template": {"labels": PURPOSE, "nodeSelector": {"kubernetes.io/arch": "amd64"},
                                  "resources": {
                         "requests": {"cpu": "2", "memory": "8Gi"},
                         "limits": {"cpu": "6", "memory": "10Gi"}}}}})

    def test_policy_contract(self):
        self.assertEqual(manifest.egress_policy("builds", "job-1"), {
            "metadata": {"atespace": "builds", "name": "default"},
            "rules": [{"tlsPassthrough": {
                "hostnames": list(manifest.PUBLIC_HOSTS), "ports": {"numbers": [443]}}},
                {"http": {"hostnames": ["deb.debian.org", "security.debian.org"],
                          "ports": {"numbers": [80]}}}]})
        self.assertIsInstance(manifest.PUBLIC_HOSTS, tuple)
        for hostname in (
            "pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "index.crates.io", "static.crates.io", "gcr.io", "github.com", "api.github.com",
            "raw.githubusercontent.com", "codeload.github.com",
            "release-assets.githubusercontent.com", "objects.githubusercontent.com",
            "pkg-containers.githubusercontent.com", "ghcr.io", "proxy.golang.org",
            "sum.golang.org", "go.dev", "dl.google.com", "storage.googleapis.com",
            "dl.k8s.io", "cdn.dl.k8s.io", "registry.k8s.io", "auth.docker.io",
            "registry-1.docker.io", "production.cloudflare.docker.com", "production.cloudfront.docker.com", "cgr.dev",
            "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
        ):
            self.assertIn(hostname, manifest.PUBLIC_HOSTS)
        result = manifest.egress_policy("builds", "job", hosts=("build.example.org",))
        self.assertEqual(result["rules"][0]["tlsPassthrough"]["hostnames"], ["build.example.org"])

    def test_invalid_names(self):
        for name in (None, 1, "", "Bad", "a.b", "a_b", "-a", "a-", "a" * 64, "a\n"):
            for call in (
                lambda: manifest.actor_template(IMAGE, name),
                lambda: manifest.actor_template(IMAGE, "builds", name),
                lambda: manifest.egress_policy(name, "job"),
                lambda: manifest.egress_policy("builds", name),
            ):
                with self.subTest(name=name), self.assertRaisesRegex(ValueError, "DNS label"):
                    call()
        for name in ("a", "0", "a" * 63):
            self.assertEqual(manifest.actor_template(IMAGE, name, name)["metadata"]["name"], name)

    def test_digest_required_for_both_images(self):
        for image in (None, 1, "", "image:latest", "image@sha256:abc", "@sha256:" + "a" * 64,
                      "image@sha256:" + "g" * 64, "https://host/image@sha256:" + "a" * 64,
                      "user:password@host/image@sha256:" + "a" * 64,
                      IMAGE + "\n", "image name@sha256:" + "a" * 64):
            for call in (manifest.worker_pool, lambda value: manifest.actor_template(value, "builds")):
                with self.subTest(image=image), self.assertRaisesRegex(ValueError, "digest"):
                    call(image)
        self.assertEqual(manifest.worker_pool("localhost:5000/work/image:v1@sha256:" + "b" * 64)
                         ["spec"]["workerImage"], "localhost:5000/work/image:v1@sha256:" + "b" * 64)

    def test_reject_unsafe_hosts(self):
        for hostname in (None, 1, "", "localhost", "*.example.com", "https://example.com",
                         "user:password@example.com", "example.com:443", "127.0.0.1", "::1",
                         "192.168.1.1", "example.com/path", "example.com\n", "a..com",
                         "host.svc.cluster.local", "service.local", "-a.example", "a" * 64 + ".org"):
            with self.subTest(hostname=hostname), self.assertRaisesRegex(ValueError, "hostname"):
                manifest.egress_policy("builds", "job", hosts=(hostname,))
        for hosts in (None, "example.com", (), tuple(f"host{i}.example.org" for i in range(257))):
            with self.assertRaisesRegex(ValueError, "host"):
                manifest.egress_policy("builds", "job", hosts=hosts)

    def test_outputs_do_not_share_mutable_state(self):
        for build in (lambda: manifest.actor_template(IMAGE, "builds"),
                      lambda: manifest.worker_pool(IMAGE),
                      lambda: manifest.egress_policy("builds", "job")):
            original = build()
            expected = json.loads(json.dumps(original))

            def corrupt(value):
                if isinstance(value, dict):
                    for child in list(value.values()):
                        corrupt(child)
                    value["corrupted"] = True
                elif isinstance(value, list):
                    for child in value:
                        corrupt(child)
                    value.append("corrupted")

            corrupt(original)
            self.assertEqual(build(), expected)
