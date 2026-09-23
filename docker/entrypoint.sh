#!/bin/sh
set -eu

LB2A_AUTH_DIR="${LB2A_AUTH_DIR:-/app/auths}"
LB2A_STATE_FILE="${LB2A_STATE_FILE:-/app/data/state.json}"
LB2A_CHECKIN_FILE="${LB2A_CHECKIN_FILE:-/app/data/checkin.json}"
LB2A_SCHEDULE_FILE="${LB2A_SCHEDULE_FILE:-/app/data/schedule.json}"
export LB2A_AUTH_DIR LB2A_STATE_FILE LB2A_CHECKIN_FILE LB2A_SCHEDULE_FILE
DATA_DIR="$(dirname "${LB2A_STATE_FILE}")"
CHECKIN_DIR="$(dirname "${LB2A_CHECKIN_FILE}")"
SCHEDULE_DIR="$(dirname "${LB2A_SCHEDULE_FILE}")"

if [ -z "${LB2A_UPSTREAM_BASE:-}" ]; then
    printf '%s\n' '启动失败：必须配置 LB2A_UPSTREAM_BASE。' >&2
    exit 1
fi
case "${LB2A_UPSTREAM_BASE}" in
    http://?*|https://?*) ;;
    *)
        printf '%s\n' '启动失败：LB2A_UPSTREAM_BASE 必须是 HTTP(S) 地址。' >&2
        exit 1
        ;;
esac

if [ ! -f "/usr/share/zoneinfo/${TZ}" ]; then
    printf '%s\n' '启动失败：TZ 不是有效的 IANA 时区。' >&2
    exit 1
fi

for dir in "${LB2A_AUTH_DIR}" "${DATA_DIR}" "${CHECKIN_DIR}" "${SCHEDULE_DIR}"; do
    mkdir -p "${dir}"
    chmod u+rwx "${dir}"
done

if [ ! -r "${LB2A_AUTH_DIR}" ] || [ ! -w "${LB2A_AUTH_DIR}" ] \
    || [ ! -w "${DATA_DIR}" ] || [ ! -w "${CHECKIN_DIR}" ] || [ ! -w "${SCHEDULE_DIR}" ]; then
    printf '%s\n' '启动失败：账号目录和状态目录必须允许容器用户读写。' >&2
    exit 1
fi

exec "$@"
