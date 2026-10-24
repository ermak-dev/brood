"""brood dev proto: generate ateapi gRPC stubs from the pinned Substrate

Reads api/ateapi/v1/ateapi.proto from the Substrate checkout (the same
SUBSTRATE_SOURCE or managed clone as brood substrate) at the commit pinned in
brood/dev/substrate/local.py and writes ateapi_pb2.py and ateapi_pb2_grpc.py
into brood/server/ateapi. The generated stubs are committed: the server
distro must not need a Substrate checkout to build. Rerun after changing the
pinned commit.
"""
import os
import subprocess
import sys
import tempfile

from brood.common import config
from brood.dev.substrate import local

PROTO = os.path.join("api", "ateapi", "v1", "ateapi.proto")
OUT = os.path.join("brood", "server", "ateapi")
# Что генератор пишет в импорт, и как зовётся пакет в brood.
GENERATED_IMPORT = "from api.ateapi.v1 import ateapi_pb2"
BROOD_IMPORT = "from brood.server.ateapi import ateapi_pb2"


def source(environ):
    """Checkout Substrate, где читать протокол: SUBSTRATE_SOURCE либо клон
    на закреплённом коммите; его отсутствие -- отказ, а не тихо пустой код."""
    path = config.get("SUBSTRATE_SOURCE")
    if not path:
        path = local.managed_checkout(environ)
        if not os.path.isdir(path):
            raise RuntimeError("Substrate is not installed: run brood substrate install "
                               "or set SUBSTRATE_SOURCE in .env")
    return local.checkout(os.path.expanduser(path))


def main(argv):
    if argv:
        raise RuntimeError("brood dev proto takes no arguments")
    checkout = source(dict(os.environ))
    import grpc_tools  # ставится extra dev: нужен только здесь

    with tempfile.TemporaryDirectory() as build:
        subprocess.run(
            [sys.executable, "-m", "grpc_tools.protoc",
             f"-I{checkout}",
             f"-I{os.path.join(os.path.dirname(grpc_tools.__file__), '_proto')}",
             "--python_out", build, "--grpc_python_out", build, PROTO],
            check=True)
        for name in ("ateapi_pb2.py", "ateapi_pb2_grpc.py"):
            with open(os.path.join(build, "api", "ateapi", "v1", name),
                      encoding="utf-8") as f:
                text = f.read()
            # Пакет стаба -- brood.server.ateapi, а не путь протофайла.
            text = text.replace(GENERATED_IMPORT, BROOD_IMPORT)
            with open(os.path.join(config.PROJECT, OUT, name), "w", encoding="utf-8") as f:
                f.write(text)
    print(f"ateapi stubs written to {OUT} from substrate {local.COMMIT[:12]}:{PROTO}")
