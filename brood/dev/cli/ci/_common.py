"""Общее глаголам CI: разбор аргументов."""
import functools

from brood.dev.cli import _common as dev_common

parser = functools.partial(dev_common.parser, "ci")
