"""Общее глаголам `brood substrate`: установка из настроек рабочей копии."""
import os
import shlex

from brood.common import config
from brood.dev.substrate import local


def cluster(environ=os.environ):
    """LocalCluster checkout'а из SUBSTRATE_SOURCE; вывод процессов -- в
    терминал командлета. Дефолта нет: путь к checkout'у -- свойство машины."""
    source = config.get("SUBSTRATE_SOURCE")
    if not source:
        raise RuntimeError("no Substrate checkout: set SUBSTRATE_SOURCE in .env to a clone "
                           "of agent-substrate/substrate")
    source = local.checkout(os.path.expanduser(source))
    return local.LocalCluster(source, local.Shell(local.environment(environ, source), source))


def no_arguments(verb, argv):
    if argv:
        raise RuntimeError(f"brood substrate {verb} takes no arguments")


def ready(c):
    """Строка готовности: чем говорить с акторами этой установки."""
    return (f"Substrate is ready: KUBECONFIG={shlex.quote(c.kubeconfig)} "
            f"{shlex.quote(c.kubectl_ate)} get actors")
