"""brood dev bug labels: the label vocabulary, one from each group per issue

A second label of the same group silently replaces the first.
"""
from brood.common.render import table
from brood.dev import gitlab


def main(_argv):
    print("\n".join(table([(g + "::", ", ".join(v)) for g, v in gitlab.VOCAB.items()])))
    print("\nOne label from each group per issue: a second one of the same "
          "group silently replaces the first.")
