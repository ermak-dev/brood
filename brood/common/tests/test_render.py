import unittest

from brood.common.render import table


class TableTests(unittest.TestCase):
    def test_aligns_all_but_last_column(self):
        self.assertEqual(table([("#1", "wip", "short"), ("#12", "-", "longer title")]), [
            "#1   wip  short",
            "#12  -    longer title",
        ])

    def test_no_trailing_spaces(self):
        self.assertEqual(table([("a", "")]), ["a"])

    def test_empty(self):
        self.assertEqual(table([]), [])
