# lobsterai2api

OpenAI-compatible API bridge for LobsterAI — multi-account pool with credit-based load balancing, SSE streaming, and automatic token refresh.

- Language: Go (zero external dependencies, pure stdlib)
- Port: `:8367` (configurable via config or `LB2A_LISTEN`)

## Architecture

```
client (OpenAI SDK)
   │ POST /v1/chat/completions (Bearer ***)
   ▼
server: pool picks account (highest credits, healthy) → check token → forward
   ▼
upstream chat API (SSE only)
   ▼ on error → classify → cooldown/disable → rotate to next account (max 3)
```

## Build

```bash
go build -o lobsterai2api.exe ./cmd/server
go build -o login.exe ./cmd/login
go build -o credit.exe ./cmd/credit
```

## Docker 一键部署

需要 Bash、Docker Engine / Docker Desktop 和 Docker Compose V2 插件（2.20 或更新版本）。宿主机无需安装 Go，首次构建需要访问 Docker Hub 和 Alpine 软件源。

在项目根目录执行：

```bash
./install.sh
```

安装器依次询问上游 API 根地址、登录门户根地址、宿主机端口、API 密钥和时区，回车采用默认值。上游地址和门户地址为必填项，留空会继续询问。首次安装默认生成随机 API 密钥，输入不回显；输入 `-` 可关闭鉴权。高级参数默认跳过。账号目录为可选项，首次检测到项目的 `auths/lobsterai-*.json` 时将其作为默认值，输入 `-` 可跳过；没有默认目录时直接回车即可跳过。

配置写入 `docker/.env`，权限为 `600`，密钥不会输出到终端。再次运行时复用已有配置，修改前备份为 `docker/.env.bak.*`，并保留额外配置项。配置值按 Compose 的规则转义，不会作为 Shell 脚本执行。

```bash
# 只生成或修改配置，不构建镜像、导入账号或启动容器
./install.sh --configure-only

# 手动部署：复制示例后至少填写 LB2A_UPSTREAM_BASE 和 LB2A_LOGIN_PORTAL，建议同时设置 LB2A_API_KEY
cp docker/.env.example docker/.env
chmod 600 docker/.env
# 编辑 docker/.env 后执行
docker compose --env-file docker/.env -f docker/compose.yaml up -d --build --wait --wait-timeout 90
```

上游地址必须使用环境变量 `LB2A_UPSTREAM_BASE`；当前服务不会读取 `config.example.json` 中的 `upstream.base_url`。填写上游根地址，不要附加 `/api` 接口路径。安装器缺少必填值时会继续询问，直接运行 Compose 或容器时则会立即报错。签到用的客户端版本解析接口 `upstream.update_url` 会被读取，空值使用官方默认地址。

### Docker 配置

| 参数 | 用途 | 默认值 / 可选值 |
|---|---|---|
| `LB2A_UPSTREAM_BASE` | 上游 API 根地址 | 必填；HTTP(S) 地址，不带查询参数或片段 |
| `LB2A_API_KEY` | `/v1/*` 接口鉴权密钥 | 安装器首次默认随机生成；空值关闭鉴权 |
| `LB2A_PORT` | 宿主机端口 | `8367`；`1–65535` |
| `LB2A_BIND_ADDRESS` | 宿主机监听 IPv4 地址 | `0.0.0.0`；仅本机访问可填 `127.0.0.1` |
| `TZ` | 签到和保活调度时区 | `Asia/Shanghai`；IANA 时区名，例如 `UTC` |
| `LB2A_TIMEOUT_SECONDS` | 上游请求超时秒数 | `180`；正整数 |
| `LB2A_HARD_CREDIT` | 积分不足冷却时间 | `12h` |
| `LB2A_SOFT_RATE` | 限流冷却时间 | `60s` |
| `LB2A_ERR_THRESHOLD` | 连续错误阈值 | `3`；正整数 |
| `LB2A_ERR_COOLDOWN` | 连续错误冷却时间 | `10m` |
| `LB2A_CHECKIN_HOURS` | 每日签到整点小时 | `9,21`；逗号分隔 `0–23`，`-` 关闭 |
| `LB2A_KEEPALIVE_HOURS` | token 保活整点小时 | `22`；格式同上 |
| `LB2A_CREDIT_REFRESH_INTERVAL` | 用户额度刷新间隔 | `30m`；支持 `30m`、`2h`、`1h30m`，`0` 或 `-` 关闭 |
| `LB2A_UPDATE_URL` | 客户端版本解析接口（签到用） | 空值使用官方默认地址 |
| `LB2A_LOGIN_PORTAL` | 管理页 OAuth 登录使用的门户根地址 | 必填；HTTP(S) 地址，不带查询参数或片段 |
| `LB2A_CALLBACK_PORT` | 授权地址里 `127.0.0.1` 回调端口 | Compose 自动取 `LB2A_PORT`，一般无需手填 |
| `COMPOSE_PROJECT_NAME` | 容器、网络、数据卷的项目前缀 | `lobsterai2api`；小写字母、数字、`_`、`-`，以字母或数字开头 |
| `LB2A_IMAGE` | 本地构建镜像名称和标签 | `lobsterai2api:local` |

冷却时间和额度刷新间隔支持 `12h`、`60s`、`1h30m` 等 Go duration 格式。容器内监听 `:8367`，账号目录固定为 `/app/auths`，状态文件为 `/app/data/state.json`，签到记录为 `/app/data/checkin.json`，定时设置为 `/app/data/schedule.json`；宿主机原有的 `LB2A_LISTEN`、`LB2A_AUTH_DIR`、`LB2A_STATE_FILE` 不改变这些容器路径。

### 账号与数据持久化

账号和状态分别保存在命名卷 `<项目名>_auths`、`<项目名>_data`，容器以 root 用户读写挂载目录。重新构建、安装或执行 `docker compose down` 会保留数据；部署后保持项目名不变才能继续使用原数据卷。

安装器从选定目录导入 `lobsterai-*.json`，源文件保持不变，导入后的文件权限为 `600`。数据卷内的同名文件会保留，避免覆盖已自动刷新的凭证。后续增加账号时再次运行 `./install.sh` 并选择账号目录，服务重建后会加载新账号。

推荐用管理页添加账号，见下方“管理页”：浏览器完成登录后粘贴返回地址，凭证直接写入容器数据卷，无需宿主机安装 Go 或 Python 3。宿主机的 `login.sh` 依然可用，但它只把账号写到宿主机 `auths/`，与容器数据卷不会自动同步，需要再次运行 `./install.sh` 导入。

容器可以在没有账号时通过 `/healthz` 健康检查，此时对话请求会返回无可用账号。`/healthz` 和 `/status` 沿用现有服务行为，无需 API 密钥。

### 运维命令

以下命令在项目根目录执行：

```bash
# 查看状态与日志
docker compose --env-file docker/.env -f docker/compose.yaml ps
docker compose --env-file docker/.env -f docker/compose.yaml logs -f --tail=100

# 更新源码后重新构建并部署；已有数据保留
docker compose --env-file docker/.env -f docker/compose.yaml up -d --build --wait --wait-timeout 90

# 查询容器中账号的积分
docker compose --env-file docker/.env -f docker/compose.yaml exec lobsterai2api credit -pretty

# 停止并删除容器，保留命名数据卷
docker compose --env-file docker/.env -f docker/compose.yaml down

# 默认端口的健康检查
curl -fsS http://127.0.0.1:8367/healthz
```

镜像采用多阶段构建，运行时包含 HTTPS 根证书和时区数据库。安装器在构建、账号导入或健康检查失败时停止，配置与已有数据卷保留。端口冲突可重新运行安装器修改宿主机端口。

## 管理页

浏览器打开 `http://127.0.0.1:8367/admin`（端口按 `LB2A_PORT` 调整），页面内嵌在二进制里，不需要额外静态资源。

添加账号的步骤：

1. 在页面顶部填入 API 密钥。密钥只存放在浏览器 `sessionStorage`，关闭标签页即失效，不会保存到服务端。
2. 点击生成授权地址，用复制按钮或新页面打开完成手机 / 微信登录。
3. 登录后浏览器会跳到 `127.0.0.1` 的回调地址。复制地址栏的完整地址，粘贴回页面提交，服务校验 `state` 后向上游交换凭证，写入账号目录并立即加入账号池。

授权地址里的回调固定为 `http://127.0.0.1:{端口}/auth/callback`，门户只接受这个路径。回调端口默认取监听端口，Docker 部署下由 Compose 取 `LB2A_PORT`，因此浏览器所在机器的这个端口需要能访问到本服务；跳转失败也不影响使用，地址栏里的 `code` 和 `state` 照样可以粘贴。远程部署时把管理页限制在可信网络内访问。

安装器要求填写 `LB2A_LOGIN_PORTAL`。手动部署时留空不影响服务启动，管理页仍可打开，只有登录接口返回配置缺失。

| 接口 | 方法 | 鉴权 | 说明 |
|---|---|---|---|
| `/admin` | GET | 无 | 管理页本体，不含任何密钥或凭证 |
| `/admin/api/accounts` | GET | API 密钥 | 账号池状态，与 `/status` 同源 |
| `/admin/api/checkins` | GET | API 密钥 | 签到记录（最新在前），含每次签到的前后积分与变化量 |
| `/admin/api/schedule` | GET | API 密钥 | 查询签到、保活和额度刷新定时设置 |
| `/admin/api/schedule` | POST | API 密钥 | 保存定时设置，立即生效并写入 `schedule_file` |
| `/admin/api/schedule` | DELETE | API 密钥 | 恢复配置文件 / 环境变量默认定时设置 |
| `/admin/api/oauth/login` | POST | API 密钥 | 生成授权地址和一次性登录会话，10 分钟有效 |
| `/admin/api/oauth/complete` | POST | API 密钥 | 提交返回地址，校验 `state` 后交换凭证并加入账号池 |
| `/auth/callback` | GET | 无 | 门户回调落到本服务时的提示页，只提示复制地址栏，不在服务端消费 `code` |

登录会话一次性消费，最多同时保留 32 个；`state` 使用常量时间比较；接口响应和日志都不包含 `code`、`state` 或任何令牌。

页面下方的“签到记录”列出每次签到的时间、账号、结果与签到前后积分变化，数据来自容器的 `/app/data/checkin.json`，随数据卷持久化。

## Login (add account)

推荐用管理页添加账号，见“管理页”。命令行方式适合本机部署，需要 Go 和 Python 3：

```bash
./login.sh
# or manually:
./login.exe url   # prints login URL (local callback server ready)
# open URL in browser → phone/WeChat login
./login.exe poll  # wait for callback → exchange → save auths/lobsterai-<uid>.json
```

## Run

```bash
./lobsterai2api.exe -config config.json
```

## Credit query

```bash
./credit.sh        # human-readable
./credit.exe       # JSON output (for scripts)
```

## Daily check-in

服务内置每日签到，替代原 Python 签到脚本：默认每天 `9:00` / `21:00` 对各账号执行签到（`schedule.checkin_hours` / `LB2A_CHECKIN_HOURS`，空列表或 `-` 关闭，关闭后启动时也不自动签到），服务启动约 5 秒后先补签一次。签到流程与客户端一致：解析客户端版本 → 查活动位 → 查活动上下文 → 执行 `check_in`；已签到、无活动为正常结果，不会报错。

每次签到前后各查一次余额，结果追加到签到记录（`checkin_file` / `LB2A_CHECKIN_FILE`，默认 `./data/checkin.json`，只保留最近 500 条）：时间、账号、状态（成功/今日已签/无活动/失败）、签到前积分、签到后积分与变化量，管理页“签到记录”区块可查看。签到后余额大于 0 的冷却账号自动解冻。

管理页“定时设置”可直接修改签到整点、token 保活整点和额度刷新间隔，保存后立即生效并写入 `schedule_file` / `LB2A_SCHEDULE_FILE`。默认每 `30m` 独立刷新一次账号额度；可填 `2h`、`1h30m`，或填 `0` / `-` 关闭。

客户端版本解析地址可用 `upstream.update_url` / `LB2A_UPDATE_URL` 覆盖，解析失败时回退内置版本号，不影响签到主流程。

## Test

```bash
# non-streaming
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer ***" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":false}'

# streaming
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer ***" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":true}'

# model list
curl -s http://127.0.0.1:8367/v1/models -H "Authorization: Bearer ***"

# status
curl -s http://127.0.0.1:8367/status
```

## Configuration

See `config.example.json`. Environment variable prefix `LB2A_*`:

| Variable | Description |
|---|---|
| `LB2A_LISTEN` | Listen address |
| `LB2A_API_KEY` | Local auth key |
| `LB2A_AUTH_DIR` | Auth file directory |
| `LB2A_STATE_FILE` | Pool state file |
| `LB2A_CHECKIN_FILE` | Check-in records file (default `./data/checkin.json`) |
| `LB2A_SCHEDULE_FILE` | Admin-managed schedule file (default `./data/schedule.json`) |
| `LB2A_CHECKIN_HOURS` | Check-in hours, comma-separated 0–23; `-` disables (default `9,21`) |
| `LB2A_KEEPALIVE_HOURS` | Token keepalive hours, same format (default `22`) |
| `LB2A_CREDIT_REFRESH_INTERVAL` | Account credit refresh interval, for example `30m` / `2h`; `0` or `-` disables (default `30m`) |
| `LB2A_HARD_CREDIT` / `LB2A_SOFT_RATE` | Cooldown durations |
| `LB2A_ERR_THRESHOLD` / `LB2A_ERR_COOLDOWN` | Error threshold and cooldown |
| `LB2A_TIMEOUT_SECONDS` | Upstream timeout |
| `LB2A_UPSTREAM_BASE` | Upstream API base URL (required) |
| `LB2A_UPDATE_URL` | Client version endpoint for check-in; empty uses official default |
| `LB2A_LOGIN_PORTAL` | Login portal URL for OAuth flow (required for login) |
| `LB2A_CALLBACK_PORT` | Callback port used in the authorization URL; defaults to the listen port |

## Features

- **Multi-account pool** — auto-load auth files from `auths/`, pick highest-credit healthy account per request
- **OpenAI-compatible** — `/v1/chat/completions` (streaming + non-streaming), `/v1/models`, `/status`, `/healthz`
- **OAuth login** — local callback server, browser-based login, auto-save credentials
- **Admin page** — `/admin` generates the authorization URL, adds an account from the pasted return address, lists the pool and check-in records
- **Token refresh** — JWT expiry parsing, proactive refresh 10min before expiry, session death auto-disable
- **Error classification** — hard credit cooldown 12h, 429 soft cooldown 60s, consecutive errors 3→10m, refresh rejected → disable
- **Request-level rotation** — up to 3 account switches per request
- **Scheduler** — daily check-in with per-account records (credits before/after), periodic credit refresh, token keepalive
- **Dynamic model list** — fetched from upstream API, cached 1h, falls back to static table

## Known limitations / TODO

- Dynamic model list from upstream API (cached 1h, falls back to static table)

## License

MIT
