"""brood dev mr merge IID [--sha SHA]: squash merge with CAS on current HEAD

Reads the current source HEAD; --sha optionally requires an expected HEAD.
Never schedules auto-merge, deletes the source branch, or retries the PUT.
After an unknown outcome, inspect show in the same project before any retry.
API-reported completion does not verify Git/main.
"""
from brood.dev import merge_requests
from brood.dev.cli._common import parser
from brood.dev.cli.mr._common import report


def main(argv):
    p = parser("mr", "merge")
    p.add_argument("iid")
    p.add_argument("--sha")
    args = p.parse_args(argv)
    data = merge_requests.merge(args.iid, sha=args.sha)
    report(data)
    if data["state"] != "merged":
        print(f"Inspect brood dev mr show {data['iid']} in the same project before any retry.")
        return 1
    return 0
