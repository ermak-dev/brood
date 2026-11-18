"""brood dev mr show IID: read a project merge request without mutation"""
from brood.dev import merge_requests
from brood.dev.cli._common import parser
from brood.dev.cli.mr._common import report


def main(argv):
    p = parser("mr", "show")
    p.add_argument("iid")
    args = p.parse_args(argv)
    report(merge_requests.show(args.iid))
    return 0
