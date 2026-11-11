"""Место на машине деплоя (#247): preflight свободного места и уборка Docker."""
import json
import unittest

from brood.dev.deploy import disk

GIB = 1 << 30
SERVER = "localhost:5002/brood-go"
EDGE = "localhost:5002/brood-go-edge"


def tag(char):
    return "build-" + char * 32


def df(available_kib, mount="/", filesystem="/dev/sda1"):
    return ("Filesystem     1024-blocks     Used Available Capacity Mounted on\n"
            f"{filesystem}  103081248 50000000 {available_kib} 54% {mount}\n")


USAGE = "\n".join(json.dumps(line) for line in (
    {"Active": "5", "Reclaimable": "10.1GB (60%)", "Size": "16.7GB", "TotalCount": "40", "Type": "Images"},
    {"Active": "2", "Reclaimable": "2MB (2%)", "Size": "91MB", "TotalCount": "7", "Type": "Containers"},
    {"Active": "0", "Reclaimable": "60.2GB", "Size": "60.2GB", "TotalCount": "2069", "Type": "Build Cache"}))


def image(repository, tag_, ident, created):
    return json.dumps({"Containers": "0", "CreatedAt": created, "CreatedSince": "1 day ago", "Digest": "<none>",
                       "ID": ident, "Repository": repository, "SharedSize": "N/A", "Size": "60MB", "Tag": tag_,
                       "UniqueSize": "N/A"})


class FakeShell:
    """Docker и df машины деплоя: ответы по префиксу команды, журнал вызовов."""

    def __init__(self, answers):
        self.answers, self.calls = answers, []

    def output(self, *argv, cwd=None):
        self.calls.append(argv)
        for prefix, answer in self.answers.items():
            if argv[:len(prefix)] == prefix:
                return answer(argv) if callable(answer) else answer
        raise AssertionError(argv)


# FEATURE: Settings of the disk guard are machine properties without defaults (#247).
# SPEC: unset means the step is skipped (previous behaviour); a set value is a
# whole number with a lower bound; anything else is refused naming the setting.
class SettingsTests(unittest.TestCase):
    def test_unset_settings_mean_no_value(self):
        self.assertEqual(disk.settings({}.get), {disk.MIN_FREE: None, disk.BUILD_CACHE: None,
                                                 disk.KEEP_SNAPSHOTS: None})
        self.assertEqual(disk.settings({disk.MIN_FREE: ""}.get)[disk.MIN_FREE], None)

    def test_whole_numbers_are_accepted_within_bounds(self):
        values = {disk.MIN_FREE: "10", disk.BUILD_CACHE: "0", disk.KEEP_SNAPSHOTS: "05"}
        self.assertEqual(disk.settings(values.get), {disk.MIN_FREE: 10, disk.BUILD_CACHE: 0,
                                                     disk.KEEP_SNAPSHOTS: 5})

    def test_malformed_values_are_refused_naming_the_setting(self):
        cases = [(disk.MIN_FREE, "0"), (disk.KEEP_SNAPSHOTS, "0"), (disk.BUILD_CACHE, "-1"),
                 (disk.MIN_FREE, "1.5"), (disk.MIN_FREE, "10GB"), (disk.BUILD_CACHE, " 5"),
                 (disk.KEEP_SNAPSHOTS, "３"), (disk.MIN_FREE, "1234567")]
        for name, raw in cases:
            with self.subTest(name=name, raw=raw), self.assertRaisesRegex(RuntimeError, name):
                disk.settings({name: raw}.get)


# FEATURE: Deploy refuses before the build when the Docker partition is nearly full (#247).
# SPEC: verdict over `df -P -k <Docker root>`: free >= limit passes; less is a
# refusal with free, limit, mount and the build cache/images usage when known.
class VerdictTests(unittest.TestCase):
    def test_enough_free_space_passes_with_the_measured_values(self):
        verdict = disk.verdict(df(42 * GIB // 1024), 10)
        self.assertEqual(verdict, {"checked": True, "mount": "/", "free": 42 * GIB, "limit": 10 * GIB})
        self.assertTrue(disk.verdict(df(10 * GIB // 1024), 10)["checked"])

    def test_low_free_space_is_refused_with_cause_and_usage(self):
        with self.assertRaises(RuntimeError) as raised:
            disk.verdict(df(3 * GIB // 1024), 10, USAGE)
        message = str(raised.exception)
        for part in ("3.0 GiB", "/", "BROOD_DEPLOY_MIN_FREE_GB=10", "build cache 60.2GB (reclaimable 60.2GB)",
                     "images 16.7GB (reclaimable 10.1GB (60%))", "docs/DEPLOY.md", disk.BUILD_CACHE):
            self.assertIn(part, message)

    def test_unreadable_usage_does_not_hide_the_free_space_refusal(self):
        for usage in ("", "not json", json.dumps({"Type": "Build Cache", "Size": 5}), "[]"):
            with self.subTest(usage=usage), self.assertRaisesRegex(RuntimeError, "1.0 GiB.*MIN_FREE_GB=2"):
                disk.verdict(df(GIB // 1024), 2, usage)

    def test_mount_points_with_spaces_are_kept_whole(self):
        self.assertEqual(disk.verdict(df(20 * GIB // 1024, mount="/mnt/docker data"), 1)["mount"],
                         "/mnt/docker data")

    def test_unreadable_df_output_is_a_refusal_naming_the_setting(self):
        for output in ("", "Filesystem 1024-blocks Used Available Capacity Mounted on\n",
                       df(5) + df(5), "garbage\nline here\n", df("many")):
            with self.subTest(output=output), self.assertRaisesRegex(RuntimeError, "cannot read free space.*MIN_FREE"):
                disk.verdict(output, 1)


class SpaceTests(unittest.TestCase):
    def shell(self, available_kib):
        return FakeShell({("docker", "info"): (0, "/var/lib/docker\n", ""),
                          ("df",): (0, df(available_kib), ""),
                          ("docker", "system", "df"): (0, USAGE, "")})

    def test_unset_limit_skips_without_touching_the_machine(self):
        shell = self.shell(1)
        self.assertEqual(disk.space(shell, {}.get), {"checked": False})
        self.assertEqual(shell.calls, [])

    def test_docker_root_partition_is_measured_and_usage_read_only_on_refusal(self):
        shell = self.shell(40 * GIB // 1024)
        verdict = disk.space(shell, {disk.MIN_FREE: "10"}.get)
        self.assertEqual(verdict["root"], "/var/lib/docker")
        self.assertEqual(verdict["free"], 40 * GIB)
        self.assertIn(("df", "-P", "-k", "/var/lib/docker"), shell.calls)
        # docker system df обходит тома и медленный: только для причины отказа.
        self.assertFalse(any(call[:3] == ("docker", "system", "df") for call in shell.calls))
        shell = self.shell(GIB // 1024)
        with self.assertRaisesRegex(RuntimeError, "build cache 60.2GB"):
            disk.space(shell, {disk.MIN_FREE: "10"}.get)

    def test_unknown_docker_root_or_df_failure_is_a_refusal(self):
        for answers in ({("docker", "info"): (1, "", "Cannot connect"), ("df",): (0, df(1), "")},
                        {("docker", "info"): (0, "\n", ""), ("df",): (0, df(1), "")},
                        {("docker", "info"): (0, "/var/lib/docker\n", ""), ("df",): (1, "", "denied")}):
            with self.subTest(answers=answers), self.assertRaisesRegex(RuntimeError, disk.MIN_FREE):
                disk.space(FakeShell(answers), {disk.MIN_FREE: "10"}.get)


# FEATURE: Old release images in the local Docker are removed after a deploy (#247).
# SPEC: only brood-go and brood-go-edge tags build-<32 hex>; per repository the
# tag of this deploy and one tag of the newest other image (the previous) stay;
# every other own tag is removed by reference; an unknown or missing current tag
# removes nothing.
class StaleImagesTests(unittest.TestCase):
    LISTING = "\n".join((
        image(SERVER, tag("a"), "id-a", "2026-11-10 12:00:00 +0000 UTC"),
        image(SERVER, tag("b"), "id-b", "2026-11-09 12:00:00 +0300 MSK"),
        image(SERVER, tag("c"), "id-c", "2026-11-08 12:00:00 +0700 +07"),
        # edge из кэша: новый тег на прежний образ, CreatedAt тот же.
        image(EDGE, tag("d"), "id-edge-1", "2026-11-01 10:00:00 +0000 UTC"),
        image(EDGE, tag("e"), "id-edge-1", "2026-11-01 10:00:00 +0000 UTC"),
        image(EDGE, tag("f"), "id-edge-0", "2026-10-01 10:00:00 +0000 UTC"),
        image(EDGE, tag("9"), "id-edge-old", "2026-09-01 10:00:00 +0000 UTC"),
        image("localhost:5002/ateworker", tag("0"), "id-worker", "2026-01-01 10:00:00 +0000 UTC"),
        image(SERVER, "latest", "id-latest", "2025-01-01 10:00:00 +0000 UTC"),
        image(EDGE, "build-short", "id-short", "2025-01-01 10:00:00 +0000 UTC"),
        image("kindest/node", "v1.37.1", "id-kind", "2025-01-01 10:00:00 +0000 UTC"),
        image("registry", "3", "id-registry", "2025-01-01 10:00:00 +0000 UTC"),
        image("<none>", "<none>", "id-dangling", "2025-01-01 10:00:00 +0000 UTC"),
        ""))
    CURRENT = (f"{SERVER}:{tag('a')}", f"{EDGE}:{tag('d')}")

    def test_keeps_current_and_previous_and_removes_only_own_old_tags(self):
        remove, keep = disk.stale_images(self.LISTING, self.CURRENT)
        self.assertEqual(keep, [f"{SERVER}:{tag('a')}", f"{SERVER}:{tag('b')}",
                                f"{EDGE}:{tag('d')}", f"{EDGE}:{tag('f')}"])
        # Тег того же образа, что текущий, только снимается: образ держит текущий тег.
        self.assertEqual(remove, [f"{SERVER}:{tag('c')}", f"{EDGE}:{tag('9')}", f"{EDGE}:{tag('e')}"])

    def test_only_the_current_image_leaves_nothing_to_remove(self):
        listing = image(SERVER, tag("a"), "id-a", "2026-11-10 12:00:00 +0000 UTC") + "\n" + \
            image(EDGE, tag("d"), "id-d", "2026-11-10 12:00:00 +0000 UTC")
        self.assertEqual(disk.stale_images(listing, self.CURRENT), ([], list(self.CURRENT)))

    def test_unknown_or_missing_current_tags_remove_nothing(self):
        for current in ((None, None), (self.CURRENT[0],), (self.CURRENT[0], f"{EDGE}:{tag('7')}"),
                        (self.CURRENT[0], "localhost:5002/brood-go-edge:latest"),
                        (self.CURRENT[0], self.CURRENT[0])):
            with self.subTest(current=current), self.assertRaisesRegex(RuntimeError, "nothing was removed"):
                disk.stale_images(self.LISTING, current)

    def test_unreadable_listing_removes_nothing(self):
        for line in ("not json", json.dumps({"Repository": SERVER}),
                     image(SERVER, tag("b"), "id-b", "yesterday")):
            with self.subTest(line=line), self.assertRaisesRegex(RuntimeError, "nothing was removed"):
                disk.stale_images(self.LISTING + "\n" + line, self.CURRENT)


class ReclaimedTests(unittest.TestCase):
    def test_total_is_read_from_both_prune_formats(self):
        self.assertEqual(disk.reclaimed("ID\tRECLAIMABLE\tSIZE\nabc\ttrue\t1GB\nTotal:\t55.2GB\n"), "55.2GB")
        self.assertEqual(disk.reclaimed("Deleted build cache objects:\nx\n\nTotal reclaimed space: 1.2GB\n"),
                         "1.2GB")
        self.assertEqual(disk.reclaimed(""), "")


class TidyTests(unittest.TestCase):
    def shell(self, rm=(0, "", ""), prune=(0, "Total:\t55.2GB\n", ""), listing=None):
        return FakeShell({("docker", "image", "ls"): (0, StaleImagesTests.LISTING if listing is None else listing, ""),
                          ("docker", "image", "rm"): rm,
                          ("docker", "builder", "prune"): prune})

    def test_unset_limit_cleans_nothing(self):
        shell = self.shell()
        self.assertEqual(disk.tidy(shell, {}.get, StaleImagesTests.CURRENT), {"enabled": False})
        self.assertEqual(shell.calls, [])

    def test_old_own_tags_are_removed_then_build_cache_is_pruned_to_the_limit(self):
        shell = self.shell()
        result = disk.tidy(shell, {disk.BUILD_CACHE: "5"}.get, StaleImagesTests.CURRENT)
        removals = [call for call in shell.calls if call[:3] == ("docker", "image", "rm")]
        self.assertEqual(removals, [("docker", "image", "rm", f"{SERVER}:{tag('c')}"),
                                    ("docker", "image", "rm", f"{EDGE}:{tag('9')}"),
                                    ("docker", "image", "rm", f"{EDGE}:{tag('e')}")])
        self.assertEqual(shell.calls[-1], ("docker", "builder", "prune", "--force", "--keep-storage", "5GB"))
        self.assertEqual(result, {"enabled": True, "cache": 5, "removed": [r[3] for r in removals],
                                  "kept": [f"{SERVER}:{tag('a')}", f"{SERVER}:{tag('b')}",
                                           f"{EDGE}:{tag('d')}", f"{EDGE}:{tag('f')}"],
                                  "reclaimed": "55.2GB", "errors": []})
        # rm по ссылке без --force (сверено выше), ни prune образов и системы: чужое не трогается.
        for call in shell.calls:
            self.assertNotIn(call[:3], {("docker", "image", "prune"), ("docker", "system", "prune")})

    def test_failures_are_reported_and_do_not_stop_the_other_step(self):
        shell = self.shell(rm=(1, "", "image is being used by running container"))
        result = disk.tidy(shell, {disk.BUILD_CACHE: "0"}.get, StaleImagesTests.CURRENT)
        self.assertEqual(result["removed"], [])
        self.assertRegex(result["errors"][0], "docker image rm .*brood-go:build-c+ failed: image is being used")
        self.assertEqual(shell.calls[-1], ("docker", "builder", "prune", "--force", "--keep-storage", "0GB"))
        shell = self.shell(listing="not json", prune=(1, "", "unknown flag: --keep-storage"))
        result = disk.tidy(shell, {disk.BUILD_CACHE: "5"}.get, StaleImagesTests.CURRENT)
        self.assertFalse(any(call[:3] == ("docker", "image", "rm") for call in shell.calls))
        self.assertEqual(len(result["errors"]), 2)
        self.assertIn("nothing was removed", result["errors"][0])
        self.assertIn("unknown flag: --keep-storage", result["errors"][1])

    def test_process_failures_are_reported_not_raised(self):
        # Выкладка уже прошла: сбой запуска docker (ENOMEM, нет бинаря) - запись в errors.
        def broken(argv):
            raise OSError(12, "Cannot allocate memory")

        shell = FakeShell({("docker", "image", "ls"): (0, StaleImagesTests.LISTING, ""),
                           ("docker", "image", "rm"): broken,
                           ("docker", "builder", "prune"): lambda argv: (_ for _ in ()).throw(
                               RuntimeError("docker is not installed"))})
        result = disk.tidy(shell, {disk.BUILD_CACHE: "5"}.get, StaleImagesTests.CURRENT)
        self.assertEqual(result["removed"], [])
        self.assertEqual(len(result["errors"]), 4)
        self.assertIn("Cannot allocate memory", result["errors"][0])
        self.assertIn("docker is not installed", result["errors"][-1])
