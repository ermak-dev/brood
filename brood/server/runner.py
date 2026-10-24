"""Тест-раннер: без меток -- все проверки пакета brood, из любого каталога.

DiscoverRunner без меток ищет от текущего каталога, и `brood server test`
не из корня рабочей копии нашёл бы ноль проверок и ответил бы зелёным.
"""
from django.test.runner import DiscoverRunner


class Runner(DiscoverRunner):
    def build_suite(self, test_labels=None, **kwargs):
        return super().build_suite(test_labels or ["brood"], **kwargs)
