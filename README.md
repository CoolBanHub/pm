# PM Process Manager

PM 是一个面向 macOS/Linux 本机环境的进程管理工具。它由常驻守护进程、Unix Socket 控制端和可选的内嵌 Web 管理后台组成，单个 Go 二进制即可运行，不依赖数据库、Node.js 或外部 Web 服务。

## 安装

macOS / Linux 一键安装（下载最新 Release，自动校验 SHA-256 后写入 `pm` 二进制，不启动守护进程、不改配置）：

```bash
curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sh
```

默认装到 `/usr/local/bin`；该目录不可写且无法 sudo 时自动回退到 `~/.local/bin` 并提示加入 PATH。安装到系统目录：

```bash
curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sudo sh
```

指定安装目录或版本：

```bash
curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sh -s -- --install-dir ~/.local/bin --version v0.0.7
```

首次安装后，后续升级用内置的 `pm update`。访问 GitHub 不畅时可设置代理：`export https_proxy=http://127.0.0.1:7897`。从源码构建见下文「构建」。

## 能力

- 进程启动、停止、重启、批量操作和状态查询
- Web 表单新增、编辑和删除进程，无需手写 YAML
- 进程分组、分组筛选、列表全选和选中项批量操作
- `never`、`unexpected`、`always` 自动重启策略
- 时间窗重启限流，持续失败后进入 `FATAL`
- 独立进程组管理，优雅停止超时后强制结束整个进程树
- CPU、内存、PID、TCP 监听端口、运行时间、进程树和重启次数监控；Go 程序可选采集 goroutine 数
- stdout/stderr 实时日志与在线日志切割
- 持久化生命周期事件历史，事件文件自动压缩
- 响应式 Web 管理后台和实时日志流
- Web 访问令牌、写操作同源校验和仅本机监听的安全默认值
- 严格配置校验、Web 配置编辑、原子替换和自动备份
- 配置 reload；不可热变更的运行参数会明确要求重启守护进程
- 前台运行、后台运行，以及 launchd/systemd 托管模板

## 构建

需要 Go 1.26 或更高版本。

```bash
make build
```

需要管理进程时，可以复制并修改示例配置：`cp pm.example.yaml pm.yaml`。不创建配置文件也可以先启动 PM。

安装到 `$GOBIN`：

```bash
make install
```

## 启动

前台运行适合调试，或者交给 launchd/systemd 托管：

```bash
./bin/pm daemon
```

未指定 `-config` 时，PM 优先使用当前工作目录下的 `pm.yaml`；不存在则使用家目录默认配置 `~/.pm/pm.yaml`。首次启动若 `~/.pm/pm.yaml` 不存在，PM 会自动创建 `~/.pm/` 并写出一份可编辑的默认配置。默认配置不含受管进程，使用 `~/.pm/pm.sock`、`~/.pm` 状态目录，并在 `127.0.0.1:19090` 启用 Web。明确指定了 `-config FILE` 时，如果文件不存在仍会报错。

指定其他配置文件：

```bash
./bin/pm daemon -config /path/to/pm.yaml
```

后台运行：

```bash
./bin/pm daemon -d
```

`-detach` 仍作为兼容别名保留；新部署请使用更简短的 `-d`。长期运行不建议使用后台模式，应该交给 launchd/systemd 托管。

Linux 上可以直接安装为 systemd 系统服务。命令会把当前 PM 二进制安装到 `/usr/local/bin/pm`，生成并启用 `pm.service`，平滑接管已运行的 daemon，并设置开机自启动：

```bash
sudo ./bin/pm systemd
```

默认使用当前目录的 `pm.yaml`；当前目录没有配置时使用发起 sudo 的用户的 `~/.pm/pm.yaml`。也可以明确指定配置：

```bash
sudo ./bin/pm systemd -config /etc/pm/pm.yaml
```

服务以发起 sudo 的用户身份运行，不会把受管程序意外切换为 root。安装后可用 `systemctl status pm` 和 `journalctl -u pm -f` 查看服务状态与 daemon 日志。

## 更新

从 GitHub 最新正式 Release 更新当前 PM 二进制：

```bash
pm update
```

PM 会先比较当前版本和最新 Release tag；版本一致时直接退出，不重复下载。需要更新时会选择当前操作系统与架构对应的产物，使用 GitHub Release asset 的 SHA-256 digest 校验（旧 Release 回退到 `SHA256SUMS`），并确认下载文件报告的版本正确后原子替换当前二进制。安装在 `/usr/local/bin/pm` 等系统目录时使用 `sudo pm update`。

更新只替换二进制文件，不会中断正在运行的 daemon。检测到正在运行的 `pm.service` 时，交互式更新会询问是否立即重启；非交互式更新会打印 `sudo systemctl restart pm.service` 提示但不会自行重启。使用 `pm up -d` 时仍需执行 `pm down` 后重新 `pm up -d`。重启 PM 会优雅停止所有受管程序，随后只自动拉起 `autostart: true` 且未暂停、未禁用的程序。

Release 手动安装请下载 `pm-<os>-<arch>.tar.gz`，归档内的 `pm` 已带 `0755` 权限。下载后直接解压即可运行，不需要额外执行 `chmod +x`（同名裸文件仅为兼容旧版 updater 保留）：

```bash
tar -xzf pm-linux-amd64.tar.gz
./pm version
```

示例配置的管理后台地址为 [http://127.0.0.1:19090](http://127.0.0.1:19090)。后台模式下，守护进程日志写入 `~/.pm/logs/pm-daemon.log`（或配置目录下的 `logs/pm-daemon.log`）。

## 命令行控制

CLI 不依赖 HTTP，所有操作都通过 Unix Socket 完成。CLI 会依次使用 `-socket`、`PM_SOCKET`、配置（当前目录 `pm.yaml` 或 `~/.pm/pm.yaml`）中的 `socket`，最后回退到内置的 `~/.pm/pm.sock`：

```bash
./bin/pm status
./bin/pm list
./bin/pm start example-worker
./bin/pm stop example-worker
./bin/pm restart example-worker
./bin/pm pause example-worker
./bin/pm resume example-worker
./bin/pm disable example-worker
./bin/pm enable example-worker
./bin/pm logs -n 100 -f example-worker
./bin/pm reload
./bin/pm shutdown
./bin/pm down      # 等价于 pm shutdown
```

`restart` 会先重新读取并校验 `pm.yaml`，再用其中的最新进程配置重启目标，因此对 `environment`、命令参数和日志路径的修改会在本次启动生效。配置文件无效时不会停止当前进程；增删进程仍使用 `pm reload`。

`pm up` 是 `pm daemon` 的短别名，参数会原样传递，因此 `pm up -d` 等价于 `pm daemon -d`。

`status` 会显示状态、持久化模式、PID、TCP 监听端口、CPU、内存、直接子进程、全部后代进程、Go goroutine、运行时间、启动次数和退出信息。`list` 是更精简的概览，显示名称、分组、状态、PID、TCP 监听端口、运行时间和描述。上述进程操作都接受多个进程名或 `all`。

`stop` 只停止当前运行实例，daemon 或机器重启后仍会按 `autostart` 再次启动。`pause` 和 `disable` 会持久化标记并停止程序，之后 PM 被 systemd 重启也不会拉起它；分别使用 `resume` 和 `enable` 清除标记并立即启动。`autostart: false` 的程序始终只手动启动。

从其他目录管理实例时，可以显式指定 Socket 或使用环境变量：

```bash
export PM_SOCKET=/absolute/path/to/run/pm.sock
pm status
```

需要让 AI 助手了解如何使用和配置 PM 时，可以直接执行 `pm llms.txt`，它会在标准输出打印一份完整的离线指南（命令、配置字段表、默认目录、示例与常见任务）：

```bash
pm llms.txt
```

## Web 安全

不需要 HTTP 管理后台时可以完全关闭，CLI 功能不受影响：

```yaml
web:
  enabled: false
```

关闭后 PM 不会创建 HTTP 监听端口，进程查询、启动、停止、重启、日志、配置 reload 和守护进程关闭仍可通过 CLI 完成。

默认只监听 `127.0.0.1`，此时访问令牌可选。只要监听地址超出本机回环地址，配置就必须提供 `token` 或 `token_env`，否则守护进程拒绝启动。推荐通过环境变量注入令牌：

```yaml
web:
  enabled: true
  listen: 0.0.0.0:19090
  token_env: PM_WEB_TOKEN
```

```bash
export PM_WEB_TOKEN="$(openssl rand -hex 24)"
```

局域网访问会以明文 HTTP 传输令牌。需要跨机器访问时，应在 PM 前放置提供 HTTPS 的反向代理，或使用 SSH 端口转发：

```bash
ssh -L 19090:127.0.0.1:19090 user@host
```

## 配置

完整配置见 [pm.example.yaml](pm.example.yaml)。配置文件中的所有相对路径均以配置文件所在目录为基准；无配置启动时则以当前工作目录为基准。

### 守护进程字段

| 字段 | 含义 | 默认值 |
| --- | --- | --- |
| `socket` | CLI 控制 Socket | `pm.sock`（→ `~/.pm/pm.sock`） |
| `state_dir` | 事件历史等运行状态目录 | `.`（→ `~/.pm`） |
| `event_history` | 内存中保留的事件数量；`0` 为关闭 | `1000` |
| `web.enabled` | 启用管理后台 | `true` |
| `web.listen` | HTTP 监听地址 | `127.0.0.1:19090` |
| `web.token` | 直接配置的访问令牌 | 空 |
| `web.token_env` | 读取访问令牌的环境变量名 | 空 |

### 进程字段

| 字段 | 含义 | 默认值 |
| --- | --- | --- |
| `name` | 唯一进程名，不允许空白字符（支持中文等 Unicode） | 必填 |
| `description` | 进程备注/介绍，不允许换行，至多 256 字符 | 空 |
| `group` | 进程分组 | `default` |
| `command` | 可执行文件；不会经过 shell 解析 | 必填 |
| `args` | 参数数组 | `[]` |
| `directory` | 工作目录 | 守护进程当前目录 |
| `environment` | 追加或覆盖环境变量 | `{}` |
| `autostart` | 守护进程启动时运行 | `true` |
| `restart` | `never`、`unexpected` 或 `always` | `unexpected` |
| `restart_delay` | 自动重启前等待时间 | `1s` |
| `max_restarts` | 时间窗内最大重启次数；`0` 为不限 | `5` |
| `restart_window` | 重启次数统计窗口 | `1m` |
| `stop_signal` | `TERM`、`INT`、`QUIT` 或 `HUP` | `TERM` |
| `stop_timeout` | 强制结束前等待时间 | `10s` |
| `stdout_log` | 标准输出日志 | 继承守护进程输出 |
| `stderr_log` | 标准错误日志；可与 stdout 使用同一文件 | 继承守护进程输出 |
| `log_max_bytes` | 单个日志文件切割阈值；`0` 为不切割 | `10485760` |
| `log_backups` | 保留的轮转日志数量 | `3` |
| `pprof_url` | Go pprof 基础地址，用于采集 goroutine 数 | 空（不采集） |

`command` 不经过 shell。需要管道、重定向或循环时，应显式使用 `"command": "/bin/sh"` 和 `"args": ["-c", "..."]`。

子进程指标由 PM 从操作系统的 PID/PPID 关系自动采集。TCP 端口仅统计处于 `LISTEN` 状态的 socket，并把主进程及全部后代进程持有的端口去重后展示；Linux 从 `/proc` 读取，macOS 通过系统 `lsof` 读取。操作系统无法仅凭 PID 得知 Go runtime 的 goroutine 数；Go 程序需要引入 `net/http/pprof`、启动仅本机可访问的 HTTP 监听，并配置 pprof 基础地址：

```yaml
pprof_url: http://127.0.0.1:6060/debug/pprof
```

PM 请求该地址下的 `/goroutine?debug=1`，单次采集超时为 750ms。生产环境不要把 pprof 端口直接暴露到公网。

Web 后台可以通过“添加进程”和进程详情中的“编辑配置”完成常用配置，不需要直接编辑 YAML。原始配置编辑器仍保留给高级场景。

后台保存配置时会先写入 `pm.yaml.bak`，再通过同目录临时文件原子替换原配置。进程配置采用差量 reload：未变化的进程保持原 PID，新增、删除或运行参数变化只影响对应进程，单纯修改分组不会重启进程。Socket、Web 监听、状态目录和事件容量变更需要重启守护进程。

## 系统托管

Linux 单用户部署优先使用 `sudo pm systemd` 自动安装。需要专用系统用户、FHS 目录规划、环境文件或更严格的安全加固时，参照完整的 [systemd 部署指南](deploy/README.md) 手动部署。macOS 本机建议使用用户级 launchd `LaunchAgent`，同一文档也包含对应说明。模板位于：

- macOS: [deploy/launchd/com.local.pm.plist.example](deploy/launchd/com.local.pm.plist.example)
- Linux: [deploy/systemd/pm.service.example](deploy/systemd/pm.service.example)

手动部署时需替换模板中的二进制、配置文件和工作目录路径。使用 launchd/systemd 托管时不要加 `-d`，由系统服务负责守护和开机启动。systemd 拉起 PM 后，PM 会启动所有 `autostart: true` 且未暂停、未禁用的程序。

## 数据文件

默认配置位于 `~/.pm/pm.yaml`，所有默认数据均落在 `~/.pm` 下（自定义配置时相对路径以配置文件所在目录为基准）：

- 配置文件：`~/.pm/pm.yaml`（首次启动自动生成）
- 控制 Socket：`~/.pm/pm.sock`
- 进程日志：由每个 program 的 `stdout_log`、`stderr_log` 决定，示例默认在 `~/.pm/logs/`
- 守护进程日志：后台模式默认 `~/.pm/logs/pm-daemon.log`
- 事件历史：`~/.pm/events.jsonl`（即 `state_dir/events.jsonl`）
- 暂停/禁用标记：`~/.pm/program-state.json`（即 `state_dir/program-state.json`）
- 配置备份：Web 保存配置时生成 `<config>.bak`

PM 不采集或上传遥测数据，Web 静态资源全部内嵌在二进制中。
