"""Представление объектов Git не меняет сборочный вход одного commit."""
import os
from pathlib import Path
import subprocess
from unittest.mock import patch

from brood.dev.build import sources
from brood.dev.build.tests.test_sources import GitCase


class PackedSourceTests(GitCase):
    def test_snapshot_canonicalizes_packed_objects(self):
        for index in range(160):
            (self.repo / f"file-{index}").write_text("shared content" * 20 + str(index))
        self.commit()
        trees = []
        with patch.dict(os.environ, {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "fetch.unpackLimit",
                                     "GIT_CONFIG_VALUE_0": "1"}):
            for level in ("0", "9"):
                cached = self.root / f"cache-{level}"
                sources.snapshot(self.repo, self.sha, cached, git_metadata=True)
                subprocess.run(["git", "-C", str(cached), "-c", f"pack.compression={level}",
                                "repack", "-a", "-d", "-F", "--window=0", "--threads=1"],
                               check=True, capture_output=True)
                target = self.root / f"snapshot-{level}"
                sources.snapshot(cached, self.sha, target, git_metadata=True)
                trees.append({str(p.relative_to(target)): p.read_bytes()
                              for p in Path(target).rglob("*") if p.is_file()})
        self.assertTrue(trees[0] == trees[1], "Git packing changed the same committed source")
