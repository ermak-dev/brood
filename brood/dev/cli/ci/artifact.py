"""brood dev ci artifact JOB ARTIFACT_PATH --out OUTPUT [--sha256 SHA]: download one job artifact

ARTIFACT_PATH is relative to the artifact archive root. OUTPUT must not exist.
SHA is a 64-digit hexadecimal SHA-256 digest and is required when the server
omits Content-Length. Redirects are refused.
"""
from brood.dev import gitlab
from brood.dev.cli.ci._common import parser


def main(argv):
    p = parser("artifact")
    p.add_argument("job", metavar="JOB")
    p.add_argument("artifact_path", metavar="ARTIFACT_PATH")
    p.add_argument("--out", required=True, metavar="OUTPUT")
    p.add_argument("--sha256", metavar="SHA")
    a = p.parse_args(argv)
    gitlab.download_artifact(a.job, a.artifact_path, a.out, a.sha256)
    print("Artifact downloaded.")
    return 0
