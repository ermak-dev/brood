"""brood dev ci retry <job> | --pipeline <id>: run a job, or a pipeline's failed jobs, again

Refused while the job or pipeline is still going, naming its state. Prints
the new job's id and url; a pipeline retry prints the pipeline's.
"""
from brood.dev import gitlab
from brood.dev.cli.ci._common import parser


def main(argv):
    p = parser("retry")
    p.add_argument("job", type=int, nargs="?")
    p.add_argument("--pipeline", type=int)
    a = p.parse_args(argv)
    if (a.job is None) == (a.pipeline is None):
        raise RuntimeError("brood dev ci retry: a job id or --pipeline <id>, one of them")
    if a.pipeline is not None:
        refused = gitlab.pipeline_retry_blocked(gitlab.pipeline_by_id(a.pipeline))
        if refused:
            raise RuntimeError(refused)
        got = gitlab.retry_pipeline(a.pipeline)
        print(f"pipeline {got['id']} {got['status']}: {got['web_url']}")
        return 0
    refused = gitlab.retry_blocked(gitlab.job(a.job))
    if refused:
        raise RuntimeError(refused)
    got = gitlab.retry_job(a.job)
    print(f"job {got['id']} {got['status']}: {got['web_url']}")
    return 0
