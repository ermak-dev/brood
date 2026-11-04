"""Контракты ограниченного образа Proxmox и безопасного build context."""
# FEATURE: Ограниченный гостевой образ и OCI-архив из чистого git HEAD.
# SPEC: Закреплённые инструменты, отсутствие секретов, отказ перезаписи и проверка DHCP.
# FEATURE: #129: гостевой образ содержит Go-клиент bro рядом с доверенным supervisor.
# SPEC: cmd/bro компилируется и извлекается через Docker; /usr/local/bin/bro исполняемый.
# IMPLEMENTATION: клиент проходит существующий compile/export/context без смены entrypoint.
# RESULT: IMPLEMENTED — оба бинарника упакованы; отсутствие bro останавливает публикацию.
# STATUS: DONE
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[4]
RECIPE = "deploy/Dockerfile.proxmox-guest"
BUILD = ".ci/proxmox-guest.sh"
DHCP = "deploy/proxmox-guest-dhcp.sh"


class ProxmoxGuestImageTests(unittest.TestCase):
    def required(self, name):
        path = ROOT / name
        self.assertTrue(path.is_file(), f"Missing guest image component: {name}")
        return path

    def test_recipe_has_pinned_tools_and_trusted_root_entrypoint(self):
        recipe = self.required(RECIPE).read_text()
        factory = (ROOT / "brood/dev/workbench/Dockerfile").read_text()
        for line in factory.splitlines():
            # claude пинает только workbench: в гостевой образ его ставит
            # bootstrap из артефактов сервера (#155), а не слой образа.
            if line.startswith(("ARG TTYD_",)):
                self.assertIn(line, recipe)
        for pin in ("CLAUDE_VERSION", "CLAUDE_SHA256"):
            self.assertIn(f'"{pin}"', (ROOT / "brood/dev/pins.json").read_text())
        self.assertNotIn("downloads.claude.ai", recipe)
        bun = next(line.split(" AS ")[0] for line in
                   (ROOT / "deploy/Dockerfile.go").read_text().splitlines()
                   if line.startswith("FROM oven/bun:"))
        self.assertIn(bun, recipe)
        self.assertNotIn(" tmux ", recipe)
        for tool in ("git", "busybox-static", "nftables", "iproute2",
                     "ca-certificates", "curl", "tini"):
            self.assertIn(tool, recipe)
        self.assertIn('ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/brood-guest"]', recipe)
        self.assertIn("CMD []", recipe)
        self.assertIn("USER 0:0", recipe)
        self.assertIn("useradd --uid 1000", recipe)
        self.assertIn("sha256sum -c -", recipe)
        self.assertIn("/usr/local/go/bin", recipe)
        self.assertNotIn("docker:", recipe)
        self.assertNotIn("kmsg", recipe)
        self.assertNotIn("COPY . ", recipe)
        self.assertNotIn("COPY go/", recipe)
        self.assertIn("DISABLE_AUTOUPDATER=1", recipe)

    # FEATURE: #163: native PTY image with a locally adapted, audited client.
    # SPEC: No tmux package; offline unprivileged source adaptation before build.
    # IMPLEMENTATION: Allowlisted patch/test files, separate upstream and derived hashes.
    # RESULT: IMPLEMENTED; captured context adds only the two required adaptation files.
    # STATUS: DONE
    def test_client_adaptation_precedes_offline_build_and_checks_derived_hash(self):
        recipe = self.required(RECIPE).read_text()
        self.assertIn("COPY driftty-direct-pty.cjs driftty-direct-pty.test.cjs /compat/", recipe)
        self.assertIn("node /compat/driftty-direct-pty.cjs .", recipe)
        self.assertIn("node /compat/driftty-direct-pty.test.cjs .", recipe)
        self.assertLess(recipe.index("USER node"), recipe.index("node /compat/driftty-direct-pty.cjs ."))
        adapted = recipe[recipe.index("RUN --network=none node /compat/driftty-direct-pty.cjs ."):]
        self.assertLess(adapted.index("node /compat/driftty-direct-pty.test.cjs ."), adapted.index("npm run build"))
        self.assertIn("RUN --network=none node /compat/driftty-direct-pty.cjs .", recipe)
        self.assertIn("npm ci --ignore-scripts --no-audit --no-fund", recipe)
        self.assertIn("ARG DRIFTTY_DIRECT_INDEX_SHA256", recipe)
        self.assertIn('"$DRIFTTY_DIRECT_INDEX_SHA256" dist/index.html', recipe)
        self.assertIn('fs.readFileSync("/source/LICENSE","utf8")', recipe)

    def test_recipe_installs_executable_bro_without_replacing_supervisor(self):
        recipe = self.required(RECIPE).read_text()
        self.assertIn("COPY bro /usr/local/bin/bro", recipe)
        chmod = next(line for line in recipe.splitlines() if line.startswith("RUN chmod 0755"))
        self.assertIn("/usr/local/bin/bro", chmod.split())
        self.assertIn("/usr/local/bin/brood-guest", chmod.split())
        self.assertIn('ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/brood-guest"]', recipe)

    def test_terminal_binary_matches_fixed_guest_runtime_path(self):
        recipe = self.required(RECIPE).read_text()
        self.assertIn("install -m 0755 /tmp/ttyd/build/ttyd /usr/bin/ttyd", recipe)

    def fixture(self, directory):
        repo = Path(directory) / "repo"
        repo.mkdir()
        for name in (RECIPE, BUILD, DHCP, ".ci/go-download.sh", "brood/dev/pins.json",
                     "deploy/Dockerfile.go", "deploy/driftty-storage.js",
                     "deploy/driftty-storage.test.cjs", "deploy/driftty-direct-pty.cjs",
                     "deploy/driftty-direct-pty.test.cjs"):
            original = self.required(name)
            target = repo / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(original, target)
        (repo / "go/cmd/brood-guest").mkdir(parents=True)
        (repo / "go/cmd/brood-guest/main.go").write_text("package main\nfunc main() {}\n")
        (repo / "go/cmd/bro").mkdir(parents=True)
        (repo / "go/cmd/bro/main.go").write_text("package main\nfunc main() {}\n")
        (repo / "go/go.mod").write_text("module fixture\ngo 1.27.0\n")
        self.git(repo, "init", "--quiet")
        self.git(repo, "add", ".")
        self.git(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "--quiet", "-m", "public source")
        tools = Path(directory) / "tools"
        tools.mkdir()
        docker = tools / "docker"
        docker.write_text('''#!/bin/bash
set -eu
printf '%s\\n' "$@" >> "$CALLS"
if [[ $1 == buildx && $2 == build ]]; then
    context=${!#}
    for arg in "$@"; do
        case "$arg" in
            type=local,dest=*)
                out=${arg#type=local,dest=}
                cp "$context/Dockerfile" "$COMPILE_RECIPE"
                printf 'guest binary' > "$out/brood-guest"
                if [[ ${OMIT_BRO:-0} != 1 ]]; then
                    printf 'client binary' > "$out/bro"
                fi
                exit 0;;
            type=oci,dest=*) output=${arg#type=oci,dest=};;
        esac
    done
    cp -a "$context/." "$CAPTURE/"
    printf 'oci archive' > "$output"
    exit 0
fi
exit 99
''')
        docker.chmod(0o755)
        capture = Path(directory) / "capture"
        capture.mkdir()
        env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}",
                   CALLS=str(Path(directory) / "calls"), CAPTURE=str(capture),
                   COMPILE_RECIPE=str(Path(directory) / "compile.Dockerfile"),
                   BUILDX_BUILDER="dedicated-builder")
        return repo, capture, env

    def git(self, repo, *args):
        return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()

    def build(self, repo, env, *args):
        return subprocess.run(["bash", str(repo / BUILD), *map(str, args)], env=env,
                              text=True, capture_output=True, cwd="/")

    def test_build_exports_oci_with_provenance_and_only_public_context(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, capture, env = self.fixture(directory)
            for name in (".env", "secrets/key", "venv/token", ".brood/operator", ".git/operator"):
                path = repo / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("operator-secret-canary")
            output = Path(directory) / "output"
            result = self.build(repo, env, output, "--seed")
            self.assertEqual(result.returncode, 0, result.stderr)
            names = {str(p.relative_to(capture)) for p in capture.rglob("*") if p.is_file()}
            self.assertEqual((capture / "brood-guest").read_text(), "guest binary")
            self.assertTrue((capture / "bro").is_file(), "Client artifact missing from guest context")
            self.assertEqual((capture / "bro").read_text(), "client binary")
            recipe = Path(env["COMPILE_RECIPE"]).read_text()
            self.assertIn('go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/bro ./cmd/bro', recipe)
            self.assertIn("COPY --from=compile /out/bro /bro", recipe)
            self.assertEqual(names, {"Dockerfile", "bro", "brood-guest", "pins.json", "go-download.sh",
                                     "proxmox-guest-dhcp.sh", "seed/project.bundle",
                                     "driftty-storage.js", "driftty-storage.test.cjs",
                                     "driftty-direct-pty.cjs", "driftty-direct-pty.test.cjs"})
            for path in capture.rglob("*"):
                if path.is_file():
                    self.assertNotIn(b"operator-secret-canary", path.read_bytes())
            bundle = capture / "seed/project.bundle"
            clone = Path(directory) / "clone"
            subprocess.run(["git", "clone", "--quiet", str(bundle), str(clone)], check=True)
            self.assertTrue((clone / "go/cmd/brood-guest/main.go").is_file())
            self.assertFalse((clone / ".env").exists())
            self.assertFalse((clone / "secrets").exists())
            self.assertNotIn("operator-secret-canary", (clone / ".git/config").read_text())
            metadata = json.loads((output / "metadata.json").read_text())
            archive = output / "proxmox-guest.tar"
            self.assertEqual(metadata["archive_sha256"], hashlib.sha256(archive.read_bytes()).hexdigest())
            self.assertEqual(metadata["source_commit"], self.git(repo, "rev-parse", "HEAD"))
            calls = Path(env["CALLS"]).read_text()
            self.assertIn("linux/amd64", calls)
            self.assertIn("type=local,dest=", calls)
            self.assertNotIn("\nrun\n", "\n" + calls)
            self.assertIn("type=oci,dest=", calls)
            self.assertIn(json.loads((repo / "brood/dev/pins.json").read_text())["PYTHON_IMAGE"], calls)
            self.assertNotIn("DOCKER_CONFIG", (repo / BUILD).read_text())

    def test_seed_strips_tracked_operator_files_and_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, capture, env = self.fixture(directory)
            for name in (".env", "secrets/key", "venv/token", ".brood/operator",
                         ".gitconfig", ".git-credentials"):
                path = repo / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("operator-secret-canary")
            self.git(repo, "add", ".")
            self.git(repo, "-c", "user.name=Private Operator", "-c",
                     "user.email=private@example.invalid", "commit", "--quiet", "-m", "operator files")
            result = self.build(repo, env, Path(directory) / "output", "--seed")
            self.assertEqual(result.returncode, 0, result.stderr)
            clone = Path(directory) / "clone"
            subprocess.run(["git", "clone", "--quiet", str(capture / "seed/project.bundle"),
                            str(clone)], check=True)
            files = self.git(clone, "ls-files")
            for name in (".env", "secrets/key", "venv/token", ".brood/operator",
                         ".gitconfig", ".git-credentials"):
                self.assertNotIn(name, files.splitlines())
            log = self.git(clone, "log", "--format=%an <%ae>")
            self.assertEqual(log, "brood <source@brood.invalid>")

    def test_build_failure_never_publishes_partial_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, _, env = self.fixture(directory)
            docker = Path(directory) / "tools/docker"
            docker.write_text("#!/bin/sh\nexit 9\n")
            output = Path(directory) / "output"
            result = self.build(repo, env, output)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(output.iterdir()), [])

    def test_missing_client_artifact_never_builds_or_publishes_guest_image(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, capture, env = self.fixture(directory)
            env["OMIT_BRO"] = "1"
            output = Path(directory) / "output"
            result = self.build(repo, env, output)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(output.iterdir()), [])
            self.assertEqual(list(capture.iterdir()), [])
            self.assertNotIn("type=oci,dest=", Path(env["CALLS"]).read_text())

    def test_missing_guest_command_refuses_before_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, _, env = self.fixture(directory)
            self.git(repo, "rm", "--quiet", "go/cmd/brood-guest/main.go")
            self.git(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                     "commit", "--quiet", "-m", "command pending")
            result = self.build(repo, env, Path(directory) / "output")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("cmd/brood-guest", result.stderr)
            self.assertFalse(Path(env["CALLS"]).exists())

    def test_seed_is_opt_in(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, capture, env = self.fixture(directory)
            result = self.build(repo, env, Path(directory) / "output")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse((capture / "seed/project.bundle").exists())

    def test_build_refuses_missing_output_dirty_source_and_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, _, env = self.fixture(directory)
            self.assertNotEqual(self.build(repo, env).returncode, 0)
            output = Path(directory) / "output"
            output.mkdir()
            canary = output / "keep"
            canary.write_text("untouched")
            self.assertNotEqual(self.build(repo, env, output).returncode, 0)
            self.assertEqual(canary.read_text(), "untouched")
            (repo / "go/go.mod").write_text("dirty")
            self.assertNotEqual(self.build(repo, env, Path(directory) / "new").returncode, 0)
            self.assertFalse(Path(env["CALLS"]).exists())

    def test_output_symlink_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, _, env = self.fixture(directory)
            output = Path(directory) / "output"
            output.symlink_to(Path(directory) / "missing")
            self.assertNotEqual(self.build(repo, env, output).returncode, 0)
            self.assertTrue(output.is_symlink())

    def dhcp(self, directory, **values):
        script = self.required(DHCP)
        tools = Path(directory) / "tools"
        tools.mkdir(exist_ok=True)
        ip = tools / "ip"
        ip.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$IP_LOG"\n')
        ip.chmod(0o755)
        resolv = Path(directory) / "resolv.conf"
        resolv.write_text("original\n")
        env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}",
                   IP_LOG=str(Path(directory) / "ip.log"), interface="eth0",
                   ip="192.0.2.10", subnet="255.255.255.0", router="192.0.2.1",
                   dns="1.1.1.1 8.8.8.8", BROOD_GUEST_RESOLV_CONF=str(resolv))
        env.update(values)
        result = subprocess.run(["sh", str(script), "bound"], env=env, text=True,
                                capture_output=True, cwd="/")
        return result, resolv, Path(env["IP_LOG"])

    def test_dhcp_configures_only_guest_interface_and_dns(self):
        with tempfile.TemporaryDirectory() as directory:
            result, resolv, log = self.dhcp(directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("addr replace 192.0.2.10/24 dev eth0", log.read_text())
            self.assertIn("route replace default via 192.0.2.1 dev eth0", log.read_text())
            self.assertEqual(resolv.read_text(), "nameserver 1.1.1.1\nnameserver 8.8.8.8\n")

    def test_dhcp_validates_entire_lease_before_first_mutation(self):
        cases = ({"ip": "192.0.2.999"}, {"ip": "01.2.3.4"}, {"subnet": "255.0.255.0"},
                 {"router": "192.0.2.1;touch /tmp/injected"}, {"dns": "1.1.1.1\nnameserver evil"},
                 {"dns": ""}, {"interface": "eth1"}, {"ip": "$(id)"},
                 {"router": "-bad"}, {"subnet": ""})
        for values in cases:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                result, resolv, log = self.dhcp(directory, **values)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(resolv.read_text(), "original\n")
                self.assertFalse(log.exists())
