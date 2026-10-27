"""brood dev test: run Python unittest tests without a server runtime.

brood dev test [DOTTED_TEST ...] [-v]

Without labels, discover every test in the brood package from any directory.
"""
import argparse
import importlib.util
from pathlib import Path
import unittest

from brood.common import config


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tests", nargs="*")
    parser.add_argument("-v", "--verbose", action="store_true")
    args = parser.parse_args(argv)
    loader = unittest.TestLoader()
    root = Path(config.PROJECT)
    if args.tests:
        suite = unittest.TestSuite()
        for name in args.tests:
            try:
                spec = importlib.util.find_spec(name)
            except (ImportError, AttributeError):
                spec = None
            if spec and spec.submodule_search_locations:
                suite.addTests(loader.discover(next(iter(spec.submodule_search_locations)),
                                               top_level_dir=str(root)))
            else:
                suite.addTests(loader.loadTestsFromName(name))
    else:
        suite = loader.discover(str(root / "brood"), top_level_dir=str(root))
    if suite.countTestCases() == 0:
        raise RuntimeError("no unittest tests found")
    result = unittest.TextTestRunner(verbosity=2 if args.verbose else 1).run(suite)
    return 0 if result.wasSuccessful() else 1
