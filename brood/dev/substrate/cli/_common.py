"""Общее глаголам `brood substrate`: установка из настроек рабочей копии."""
import os
import shlex

from brood.common import config
from brood.dev.substrate import local


def cluster(environ=os.environ, fetch=False):
    """LocalCluster checkout'а; вывод процессов -- в терминал командлета.

    SUBSTRATE_SOURCE -- клон разработчика, его не трогаем. Без него -- клон
    официального репозитория в каталоге данных: fetch (только install)
    заводит его и переводит на закреплённый коммит, остальные глаголы берут
    как есть и в сеть не ходят."""
    source = config.get("SUBSTRATE_SOURCE")
    if source:
        source = os.path.expanduser(source)
    else:
        source = local.managed_checkout(environ)
        if fetch:
            local.sync(local.Shell(dict(environ), None), source)
        elif not os.path.isdir(source):
            raise RuntimeError("Substrate is not installed: run brood substrate install")
    source = local.checkout(source)
    return local.LocalCluster(source, local.Shell(local.environment(environ, source), source),
                              ports=local.edge_ports(config.get))


def no_arguments(verb, argv):
    if argv:
        raise RuntimeError(f"brood substrate {verb} takes no arguments")


def ready(c):
    """Строка готовности: чем говорить с акторами этой установки."""
    return (f"Substrate is ready: KUBECONFIG={shlex.quote(c.kubeconfig)} "
            f"{shlex.quote(c.kubectl_ate)} get actors")
