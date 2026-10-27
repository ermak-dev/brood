"""brood deploy server: check readiness for a public Go deployment

Public cutover is blocked until an operator has prepared imported users,
recovery access and a Go-only TLS edge. No cluster resources are changed.
Use brood deploy go to prepare the private Go endpoint independently.
"""


def main(argv):
    if argv not in ([], ["--check"]):
        raise RuntimeError("brood deploy server accepts only --check")
    # Нет автоматического доказательства миграции и переключения публичного
    # входа. Флаг из .env не доказывает, что пустая SQLite готова к cutover.
    raise RuntimeError(
        "Go public cutover is not prepared: migrate and verify users, recovery access and a Go-only TLS edge; "
        "automatic public deployment is blocked before cluster changes. "
        "Use brood deploy go only to prepare the private endpoint; it does not import users or switch traffic."
    )
