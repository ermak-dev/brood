"""Go build-stage не копирует ненужный полный Python SDK через VFS."""
# BUG: #86: полная Python-база размножается в VFS-слоях Go image build.
# HYPOTHESIS: slim build-only база с явными curl/CA сохраняет тот же Go runtime.
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[4]


class GoBuildBaseTests(unittest.TestCase):
    def test_build_only_base_is_a_pinned_python_slim_image(self):
        recipe = (ROOT / "deploy/Dockerfile.go").read_text()
        self.assertRegex(recipe.splitlines()[0],
                         r"^ARG PYTHON_IMAGE=python:3\.12-slim@sha256:[0-9a-f]{64}$")
        self.assertIn("FROM ${PYTHON_IMAGE} AS build", recipe)
        self.assertIn("apt-get install -y --no-install-recommends ca-certificates curl", recipe)
        self.assertIn("rm -rf /var/lib/apt/lists/*", recipe)

    def test_minimal_base_does_not_change_compiler_integrity_or_scratch_runtime(self):
        recipe = (ROOT / "deploy/Dockerfile.go").read_text()
        self.assertIn('p["GO_VERSION"]', recipe)
        self.assertIn('p["GO_SHA256"]', recipe)
        self.assertIn('"$checksum" /tmp/go.tar.gz | sha256sum -c -', recipe)
        self.assertIn("CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off", recipe)
        runtime = recipe.split("FROM scratch\n", 1)[1]
        self.assertNotIn("RUN ", runtime)
        self.assertNotIn("python", runtime)
        self.assertIn("USER 65532:65532", runtime)
        self.assertEqual(re.findall(r"^COPY .*$", runtime, re.MULTILINE), [
            "COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
            "COPY --from=build /out/brood /brood",
            "COPY --from=build /out/bro /bro",
            "COPY --from=build --chown=65532:65532 /out/data /data"])
