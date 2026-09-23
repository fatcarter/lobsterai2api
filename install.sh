#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DOCKER_DIR="${ROOT_DIR}/docker"
ENV_FILE="${DOCKER_DIR}/.env"
COMPOSE_FILE="${DOCKER_DIR}/compose.yaml"
CONFIGURE_ONLY=false
TEMP_ENV=""
ANSWER=""
API_KEY_CONFIGURED="${LB2A_API_KEY+x}"

fail() {
    printf '安装失败：%s\n' "$1" >&2
    exit 1
}

cleanup() {
    if [[ -n "${TEMP_ENV}" && -f "${TEMP_ENV}" ]]; then
        rm -f -- "${TEMP_ENV}"
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

case "${1:-}" in
    "") ;;
    --configure-only) CONFIGURE_ONLY=true ;;
    -h|--help)
        printf '%s\n' '用法：./install.sh [--configure-only]' \
            '默认交互配置、构建镜像、导入可选账号并启动服务。' \
            '--configure-only  仅保存配置，不构建镜像或启动容器。' \
            '回车接受默认值；可选项输入 - 清空或跳过。'
        exit 0
        ;;
    *) fail '未知参数，使用 --help 查看用法。' ;;
esac
[[ $# -le 1 ]] || fail '参数过多，使用 --help 查看用法。'

command -v docker >/dev/null 2>&1 || fail '请先安装 Docker Engine 或 Docker Desktop。'
docker compose version >/dev/null 2>&1 || fail '请先安装 Docker Compose V2 插件（2.20 或更新版本）。'
COMPOSE_UP_HELP="$(docker compose up --help)"
[[ "${COMPOSE_UP_HELP}" == *--wait-timeout* ]] || fail 'Docker Compose 版本过旧，需要 2.20 或更新版本。'
if [[ "${CONFIGURE_ONLY}" == false ]]; then
    docker info >/dev/null 2>&1 || fail '无法连接 Docker，请启动 Docker 并确认当前用户有访问权限。'
fi

COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-lobsterai2api}"
LB2A_IMAGE="${LB2A_IMAGE:-lobsterai2api:local}"
LB2A_UPSTREAM_BASE="${LB2A_UPSTREAM_BASE:-}"
LB2A_LOGIN_PORTAL="${LB2A_LOGIN_PORTAL:-}"
LB2A_API_KEY="${LB2A_API_KEY:-}"
LB2A_PORT="${LB2A_PORT:-8367}"
LB2A_BIND_ADDRESS="${LB2A_BIND_ADDRESS:-0.0.0.0}"
TZ="${TZ:-Asia/Shanghai}"
LB2A_TIMEOUT_SECONDS="${LB2A_TIMEOUT_SECONDS:-180}"
LB2A_HARD_CREDIT="${LB2A_HARD_CREDIT:-12h}"
LB2A_SOFT_RATE="${LB2A_SOFT_RATE:-60s}"
LB2A_ERR_THRESHOLD="${LB2A_ERR_THRESHOLD:-3}"
LB2A_ERR_COOLDOWN="${LB2A_ERR_COOLDOWN:-10m}"
LB2A_CHECKIN_HOURS="${LB2A_CHECKIN_HOURS:-9,21}"
LB2A_KEEPALIVE_HOURS="${LB2A_KEEPALIVE_HOURS:-22}"
LB2A_CREDIT_REFRESH_INTERVAL="${LB2A_CREDIT_REFRESH_INTERVAL:-30m}"
LB2A_UPDATE_URL="${LB2A_UPDATE_URL:-}"

# 使用 Compose 自己解析 .env，保留其引号和插值语义，绝不执行配置中的 Shell 内容。
if [[ -f "${ENV_FILE}" ]]; then
    # Compose 的解析错误可能包含原始密钥，只返回不含配置值的错误提示。
    RESOLVED_ENV="$(docker compose --env-file "${ENV_FILE}" -f - config --environment 2>/dev/null <<'YAML'
name: lobsterai2api
services:
  config:
    image: scratch
YAML
    )" || fail '已有 docker/.env 无法解析，请检查键值格式和引号后重试；原配置未修改。'
    while IFS='=' read -r key value; do
        case "${key}" in
            COMPOSE_PROJECT_NAME|LB2A_IMAGE|LB2A_UPSTREAM_BASE|LB2A_LOGIN_PORTAL|LB2A_API_KEY|LB2A_PORT|LB2A_BIND_ADDRESS|TZ|LB2A_TIMEOUT_SECONDS|LB2A_HARD_CREDIT|LB2A_SOFT_RATE|LB2A_ERR_THRESHOLD|LB2A_ERR_COOLDOWN|LB2A_CHECKIN_HOURS|LB2A_KEEPALIVE_HOURS|LB2A_CREDIT_REFRESH_INTERVAL|LB2A_UPDATE_URL)
                printf -v "${key}" '%s' "${value}"
                if [[ "${key}" == LB2A_API_KEY ]]; then API_KEY_CONFIGURED=x; fi
                ;;
        esac
    done <<< "${RESOLVED_ENV}"
    unset RESOLVED_ENV
    printf '%s\n' '已加载 docker/.env，回车保留当前配置。'
fi

read_answer() {
    printf '%s: ' "$1" >&2
    if [[ "${2:-}" == secret ]]; then
        if ! IFS= read -r -s ANSWER; then fail '输入已结束，配置尚未保存。'; fi
        printf '\n' >&2
    else
        if ! IFS= read -r ANSWER; then fail '输入已结束，配置尚未保存。'; fi
    fi
}

valid_port() {
    [[ "$1" =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

valid_positive_integer() {
    [[ "$1" =~ ^[0-9]{1,9}$ ]] && (( 10#$1 > 0 ))
}

valid_url() {
    local pattern='^https?://([a-zA-Z0-9][a-zA-Z0-9.-]*|\[[0-9a-fA-F:]+\])(:[0-9]{1,5})?(/[^[:space:]?#]*)?$'
    local authority
    [[ "$1" =~ ${pattern} ]] || return 1
    authority="${1#*://}"
    authority="${authority%%/*}"
    if [[ "${authority}" =~ :([0-9]+)$ ]]; then
        valid_port "${BASH_REMATCH[1]}" || return 1
    fi
}

valid_key() {
    [[ "$1" != *[[:space:][:cntrl:]]* ]]
}

valid_timezone() {
    [[ "$1" =~ ^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$ ]] || return 1
    if [[ -d /usr/share/zoneinfo ]]; then
        [[ -f "/usr/share/zoneinfo/$1" ]] || return 1
    fi
}

valid_ipv4() {
    local octet
    local -a octets
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS=. read -r -a octets <<< "$1"
    for octet in "${octets[@]}"; do
        (( 10#${octet} <= 255 )) || return 1
        [[ "${octet}" == 0 || "${octet}" != 0* ]] || return 1
    done
}

valid_duration() {
    [[ "$1" =~ ^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$ && ${#1} -le 64 ]]
}

valid_interval() {
    local hours minutes
    [[ "$1" == - || "$1" == 0 ]] && return 0
    [[ "$1" =~ ^([0-9]+h)?([0-9]+m)?$ ]] || return 1
    [[ -n "${BASH_REMATCH[1]}${BASH_REMATCH[2]}" ]] || return 1
    hours="${BASH_REMATCH[1]%h}"
    minutes="${BASH_REMATCH[2]%m}"
    hours="${hours:-0}"
    minutes="${minutes:-0}"
    (( 10#${hours} > 0 || 10#${minutes} >= 1 ))
}

valid_hours() {
    local IFS=, hour
    [[ "$1" == - ]] && return 0
    [[ "$1" =~ ^[0-9]{1,2}(,[0-9]{1,2})*$ ]] || return 1
    for hour in $1; do
        (( 10#$hour <= 23 )) || return 1
    done
}

valid_auth_dir() {
    local file found=false
    [[ -z "$1" ]] && return 0
    [[ -d "$1" && -r "$1" ]] || return 1
    for file in "$1"/lobsterai-*.json; do
        [[ -e "${file}" ]] || continue
        [[ -f "${file}" && -r "${file}" && ! -L "${file}" ]] || return 1
        found=true
    done
    [[ "${found}" == true ]]
}

prompt_setting() {
    local name="$1" label="$2" validator="$3" optional="${4:-false}" input
    while true; do
        read_answer "${label} [${!name:-无}]"
        input="${ANSWER:-${!name}}"
        if [[ "${optional}" == true && "${input}" == - ]]; then input=""; fi
        if "${validator}" "${input}"; then
            printf -v "${name}" '%s' "${input}"
            return
        fi
        printf '%s\n' '输入无效，请按提示重新填写。' >&2
    done
}

printf '%s\n' 'LobsterAI Docker 安装：回车使用默认值，可选项输入 - 跳过。'
prompt_setting LB2A_UPSTREAM_BASE '上游 API 根地址（必填，http:// 或 https://）' valid_url
LB2A_UPSTREAM_BASE="${LB2A_UPSTREAM_BASE%/}"
# 门户地址决定管理页生成的 OAuth 授权地址，缺失则无法通过页面添加账号，因此按必填处理。
prompt_setting LB2A_LOGIN_PORTAL '登录门户根地址（必填，管理页登录用）' valid_url
LB2A_LOGIN_PORTAL="${LB2A_LOGIN_PORTAL%/}"
prompt_setting LB2A_PORT '宿主机端口（1–65535）' valid_port

KEY_HINT='自动生成随机密钥；输入 - 关闭鉴权'
if [[ -n "${LB2A_API_KEY}" ]]; then
    KEY_HINT='已设置，回车保留；输入 - 关闭鉴权'
elif [[ -n "${API_KEY_CONFIGURED}" ]]; then
    KEY_HINT='已关闭鉴权，回车保留'
fi
while true; do
    read_answer "API 密钥（输入不回显）[${KEY_HINT}]" secret
    if [[ "${ANSWER}" == - ]]; then
        LB2A_API_KEY=""
    elif [[ -n "${ANSWER}" ]]; then
        LB2A_API_KEY="${ANSWER}"
    elif [[ -z "${API_KEY_CONFIGURED}" && -z "${LB2A_API_KEY}" ]]; then
        LB2A_API_KEY="sk-$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
    fi
    valid_key "${LB2A_API_KEY}" && break
    printf '%s\n' '密钥不能包含空白或控制字符，请重新填写。' >&2
done
prompt_setting TZ '时区' valid_timezone

while true; do
    read_answer '配置高级参数（监听地址、超时、冷却）[y/N]'
    case "${ANSWER}" in
        y|Y|yes|YES)
            prompt_setting LB2A_BIND_ADDRESS '宿主机监听 IPv4 地址' valid_ipv4
            prompt_setting LB2A_TIMEOUT_SECONDS '上游超时秒数（正整数）' valid_positive_integer
            prompt_setting LB2A_HARD_CREDIT '积分不足冷却（例如 12h）' valid_duration
            prompt_setting LB2A_SOFT_RATE '限流冷却（例如 60s）' valid_duration
            prompt_setting LB2A_ERR_THRESHOLD '连续错误阈值（正整数）' valid_positive_integer
            prompt_setting LB2A_ERR_COOLDOWN '连续错误冷却（例如 10m）' valid_duration
            prompt_setting LB2A_CREDIT_REFRESH_INTERVAL '额度刷新间隔（30m、2h、0 关闭）' valid_interval
            break
            ;;
        ""|n|N|no|NO) break ;;
        *) printf '%s\n' '请输入 y 或 n。' >&2 ;;
    esac
done

valid_ipv4 "${LB2A_BIND_ADDRESS}" || fail '监听地址无效，请重新运行并配置高级参数。'
valid_positive_integer "${LB2A_TIMEOUT_SECONDS}" || fail '超时必须是正整数。'
valid_positive_integer "${LB2A_ERR_THRESHOLD}" || fail '错误阈值必须是正整数。'
for key in LB2A_HARD_CREDIT LB2A_SOFT_RATE LB2A_ERR_COOLDOWN; do
    valid_duration "${!key}" || fail "${key} 格式无效，请重新运行并配置高级参数。"
done
for key in LB2A_CHECKIN_HOURS LB2A_KEEPALIVE_HOURS; do
    valid_hours "${!key}" || fail "${key} 必须是 0-23 的逗号分隔小时列表（- 表示关闭）。"
done
valid_interval "${LB2A_CREDIT_REFRESH_INTERVAL}" || fail 'LB2A_CREDIT_REFRESH_INTERVAL 必须是 30m、2h、1h30m，或 0/- 关闭。'
[[ "${COMPOSE_PROJECT_NAME}" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || fail 'COMPOSE_PROJECT_NAME 只能使用小写字母、数字、下划线和连字符，并以字母或数字开头。'

AUTH_IMPORT_DIR=""
if [[ "${CONFIGURE_ONLY}" == false ]]; then
    # 重装时默认跳过导入，避免用宿主机的旧文件覆盖容器已刷新的凭证。
    if [[ ! -f "${ENV_FILE}" ]] && valid_auth_dir "${ROOT_DIR}/auths"; then
        AUTH_IMPORT_DIR="${ROOT_DIR}/auths"
    fi
    prompt_setting AUTH_IMPORT_DIR '账号文件目录（含 lobsterai-*.json，可选，输入 - 跳过）' valid_auth_dir true
fi

TEMP_ENV="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
if [[ -f "${ENV_FILE}" ]]; then
    # 保留用户额外添加的配置和注释，只替换安装器管理的字段。
    awk '!/^[[:space:]]*(export[[:space:]]+)?(COMPOSE_PROJECT_NAME|LB2A_IMAGE|LB2A_UPSTREAM_BASE|LB2A_LOGIN_PORTAL|LB2A_API_KEY|LB2A_PORT|LB2A_BIND_ADDRESS|TZ|LB2A_TIMEOUT_SECONDS|LB2A_HARD_CREDIT|LB2A_SOFT_RATE|LB2A_ERR_THRESHOLD|LB2A_ERR_COOLDOWN|LB2A_CHECKIN_HOURS|LB2A_KEEPALIVE_HOURS|LB2A_CREDIT_REFRESH_INTERVAL|LB2A_UPDATE_URL)[[:space:]]*=/' \
        "${ENV_FILE}" > "${TEMP_ENV}"
else
    printf '%s\n' '# install.sh 生成；包含敏感配置，请勿提交。' > "${TEMP_ENV}"
fi
for key in COMPOSE_PROJECT_NAME LB2A_IMAGE LB2A_UPSTREAM_BASE LB2A_LOGIN_PORTAL LB2A_API_KEY LB2A_PORT LB2A_BIND_ADDRESS TZ LB2A_TIMEOUT_SECONDS LB2A_HARD_CREDIT LB2A_SOFT_RATE LB2A_ERR_THRESHOLD LB2A_ERR_COOLDOWN LB2A_CHECKIN_HOURS LB2A_KEEPALIVE_HOURS LB2A_CREDIT_REFRESH_INTERVAL LB2A_UPDATE_URL; do
    value="${!key}"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//\$/\$\$}"
    printf '%s="%s"\n' "${key}" "${value}" >> "${TEMP_ENV}"
done
docker compose --project-directory "${DOCKER_DIR}" --env-file "${TEMP_ENV}" -f "${COMPOSE_FILE}" config --quiet 2>/dev/null \
    || fail '生成的 Compose 配置校验失败，原配置未修改。'

if [[ -f "${ENV_FILE}" ]] && cmp -s "${ENV_FILE}" "${TEMP_ENV}"; then
    rm -f -- "${TEMP_ENV}"
else
    if [[ -f "${ENV_FILE}" ]]; then
        BACKUP_FILE="$(mktemp "${ENV_FILE}.bak.XXXXXX")"
        cat "${ENV_FILE}" > "${BACKUP_FILE}"
        printf '原配置已备份：%s\n' "${BACKUP_FILE}"
    fi
    mv -f -- "${TEMP_ENV}" "${ENV_FILE}"
fi
TEMP_ENV=""
chmod 600 "${ENV_FILE}"
printf '配置已保存：%s（权限 600；密钥仅保存在配置中）\n' "${ENV_FILE}"
if [[ "${CONFIGURE_ONLY}" == true ]]; then exit 0; fi

compose() {
    docker compose --project-directory "${DOCKER_DIR}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" "$@"
}

printf '%s\n' '正在构建 Docker 镜像……'
compose build || fail '镜像构建失败，已保存配置；修复构建问题后重新运行安装脚本。'

if [[ -n "${AUTH_IMPORT_DIR}" ]]; then
    AUTH_FILES=()
    for file in "${AUTH_IMPORT_DIR}"/lobsterai-*.json; do AUTH_FILES+=("${file##*/}"); done
    printf '%s\n' '正在导入账号，数据卷中的同名文件将保留……'
    # 变量由容器内的 Shell 展开，单引号避免宿主机提前替换路径。
    # shellcheck disable=SC2016
    COPYFILE_DISABLE=1 tar -C "${AUTH_IMPORT_DIR}" -cf - -- "${AUTH_FILES[@]}" \
        | compose run --rm --no-deps -T --entrypoint sh lobsterai2api -eu -c '
            umask 077
            stage=$(mktemp -d)
            trap '\''rm -rf "${stage}"'\'' EXIT
            tar -xf - -C "${stage}" --no-same-owner
            imported=0
            skipped=0
            for file in "${stage}"/lobsterai-*.json; do
                [ -f "${file}" ] || continue
                target="/app/auths/${file##*/}"
                if [ -e "${target}" ]; then
                    skipped=$((skipped + 1))
                else
                    cp "${file}" "${target}"
                    chmod 600 "${target}"
                    imported=$((imported + 1))
                fi
            done
            printf "账号导入完成：新增 %s，保留已有 %s。\n" "${imported}" "${skipped}"
        ' || fail '账号导入失败，服务尚未重建；检查目录和 Docker 输出后重试。'
fi

printf '%s\n' '正在启动服务并等待健康检查……'
compose up -d --force-recreate --wait --wait-timeout 90 lobsterai2api \
    || fail '服务未通过健康检查，请用 docker compose --env-file docker/.env -f docker/compose.yaml logs --tail=100 查看日志。'

ACCESS_HOST="${LB2A_BIND_ADDRESS}"
if [[ "${ACCESS_HOST}" == 0.0.0.0 ]]; then ACCESS_HOST=127.0.0.1; fi
printf '部署完成。API 地址：http://%s:%s/v1\n' "${ACCESS_HOST}" "${LB2A_PORT}"
printf '管理页：http://%s:%s/admin\n' "${ACCESS_HOST}" "${LB2A_PORT}"
printf '%s\n' '健康检查通过；尚未导入账号时，对话请求会返回无可用账号。' \
    '查看日志：docker compose --env-file docker/.env -f docker/compose.yaml logs -f'
