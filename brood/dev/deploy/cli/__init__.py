"""the brood server in the local Substrate cluster: image, Postgres and a TLS edge

  brood deploy server    build the server image, deploy it with Postgres into namespace brood

Deploys into the cluster of brood substrate install, with its ownership
checks. Settings come from the working copy's .env: DJANGO_SECRET_KEY,
DJANGO_ALLOWED_HOSTS, POSTGRES_PASSWORD, DJANGO_SUPERUSER_USERNAME,
DJANGO_SUPERUSER_PASSWORD and optionally DJANGO_SUPERUSER_EMAIL. The TLS
certificate and key are secrets/tls.crt and secrets/tls.key. With
BROOD_HTTPS_PORT set, brood substrate install publishes the server on that
host port, and BROOD_HTTP_PORT redirects to it. See docs/DEPLOY.md.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
