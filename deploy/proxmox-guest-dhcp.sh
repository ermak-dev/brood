#!/bin/sh
# Hook busybox udhcpc; запускается только доверенным root supervisor внутри гостя.
set -eu
set -f
fail() { printf '%s\n' 'Invalid guest DHCP lease' >&2; exit 1; }
ipv4() (
    case "$1" in ''|*[!0-9.]*|.*|*.|*..*) exit 1;; esac
    IFS=.
    # Разделение октетов намеренное; globbing отключён выше.
    # shellcheck disable=SC2086
    set -- $1
    [ "$#" -eq 4 ] || exit 1
    for octet do
        case "$octet" in ''|0[0-9]*) exit 1;; esac
        [ "${#octet}" -le 3 ] && [ "$octet" -le 255 ] || exit 1
    done
)
unicast() (
    ipv4 "$1" || exit 1
    first=${1%%.*}
    [ "$first" -gt 0 ] && [ "$first" -lt 224 ] && [ "$first" -ne 127 ]
)
prefix() (
    ipv4 "$1" || exit 1
    IFS=.
    # Разделение октетов намеренное; globbing отключён выше.
    # shellcheck disable=SC2086
    set -- $1
    bits=0 ended=0
    for octet do
        if [ "$ended" -eq 1 ]; then
            [ "$octet" -eq 0 ] || exit 1
            continue
        fi
        case "$octet" in
            255) bits=$((bits + 8));;
            254) bits=$((bits + 7)); ended=1;;
            252) bits=$((bits + 6)); ended=1;;
            248) bits=$((bits + 5)); ended=1;;
            240) bits=$((bits + 4)); ended=1;;
            224) bits=$((bits + 3)); ended=1;;
            192) bits=$((bits + 2)); ended=1;;
            128) bits=$((bits + 1)); ended=1;;
            0) ended=1;;
            *) exit 1;;
        esac
    done
    printf '%s\n' "$bits"
)
[ "${interface:-}" = eth0 ] || fail
case "${1:-}" in
    deconfig)
        ip -4 addr flush dev eth0 scope global
        exit 0;;
    bound|renew) ;;
    *) printf '%s\n' 'Unsupported guest DHCP event' >&2; exit 1;;
esac
unicast "${ip:-}" || fail
mask=$(prefix "${subnet:-}") || fail
# Списки содержат только адреса и пробелы; проверяем весь lease до первого ip.
for list in "${router:-}" "${dns:-}"; do
    case "$list" in ''|*[!0-9.\ ]*) fail;; esac
    # shellcheck disable=SC2086
    set -- $list
    [ "$#" -gt 0 ] || fail
    [ "$#" -le 8 ] || fail
    for address do unicast "$address" || fail; done
done
# shellcheck disable=SC2086
set -- $router
gateway=$1
resolv=${BROOD_GUEST_RESOLV_CONF:-/etc/resolv.conf}
case "$resolv" in /*) ;; *) fail;; esac
[ ! -L "$resolv" ] || fail
temporary=$(mktemp "${resolv}.XXXXXX")
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
for address in $dns; do printf 'nameserver %s\n' "$address"; done > "$temporary"
chmod 0644 "$temporary"
ip link set dev eth0 up
ip -4 addr flush dev eth0 scope global
ip addr replace "$ip/$mask" dev eth0
ip route replace default via "$gateway" dev eth0
# Запись сохраняет mountpoint resolv.conf, если runtime монтирует его отдельно.
cat "$temporary" > "$resolv"
