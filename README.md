# PM Process Manager

PM 是一个面向 macOS/Linux 本机环境的进程管理工具。它由常驻守护进程、Unix Socket 控制端和可选的内嵌 Web 管理后台组成，单个 Go 二进制即可运行，不依赖数据库、Node.js 或外部 Web 服务。

## 能力

- 进程启动、停止、重启、批量操作和状态查询
- Web 表单新增、编辑和删除进程，无需手写 YAML
- 进程分组、分组筛选、列表全选和选中项批量操作
- `never`、`unexpected`、`always` 自动重启策略
- 时间窗重启限流，持续失败后进入 `FATAL`
- 独立进程组管理，优雅停止超时后强制结束整个进程树
- CPU、内存、PID、运行时间、进程树和重启次数监控；Go 程序可选采集 goroutine 数
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

示例配置的管理后台地址为 [http://127.0.0.1:19090](http://127.0.0.1:19090)。后台模式下，守护进程日志写入 `~/.pm/logs/pm-daemon.log`（或配置目录下的 `logs/pm-daemon.log`）。

## 命令行控制

CLI 不依赖 HTTP，所有操作都通过 Unix Socket 完成。CLI 会依次使用 `-socket`、`PM_SOCKET`、配置（当前目录 `pm.yaml` 或 `~/.pm/pm.yaml`）中的 `socket`，最后回退到内置的 `~/.pm/pm.sock`：

```bash
./bin/pm status
./bin/pm list
./bin/pm start example-worker
./bin/pm stop example-worker
./bin/pm restart example-worker
./bin/pm logs -n 100 -f example-worker
./bin/pm reload
./bin/pm shutdown
```

`status` 会显示状态、PID、CPU、内存、直接子进程、全部后代进程、Go goroutine、运行时间、启动次数和退出信息。`list` 是更精简的概览，仅显示名称、分组、状态、PID 和运行时间。`start`、`stop`、`restart` 接受多个进程名或 `all`。从其他目录管理实例时，可以显式指定 Socket 或使用环境变量：

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
| `name` | 唯一进程名，不允许空白字符 | 必填 |
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

子进程指标由 PM 从操作系统的 PID/PPID 关系自动采集。操作系统无法仅凭 PID 得知 Go runtime 的 goroutine 数；Go 程序需要引入 `net/http/pprof`、启动仅本机可访问的 HTTP 监听，并配置 pprof 基础地址：

```yaml
pprof_url: http://127.0.0.1:6060/debug/pprof
```

PM 请求该地址下的 `/goroutine?debug=1`，单次采集超时为 750ms。生产环境不要把 pprof 端口直接暴露到公网。

Web 后台可以通过“添加进程”和进程详情中的“编辑配置”完成常用配置，不需要直接编辑 YAML。原始配置编辑器仍保留给高级场景。

后台保存配置时会先写入 `pm.yaml.bak`，再通过同目录临时文件原子替换原配置。进程配置采用差量 reload：未变化的进程保持原 PID，新增、删除或运行参数变化只影响对应进程，单纯修改分组不会重启进程。Socket、Web 监听、状态目录和事件容量变更需要重启守护进程。

## 系统托管

长期运行推荐使用系统服务，而不是 `-d`。完整的 Linux systemd 部署指南（目录规划、专用用户、生产配置、单元文件、加固与运维）见 [deploy/README.md](deploy/README.md)。macOS 本机建议使用用户级 launchd `LaunchAgent`，同一文档也包含对应说明。模板位于：

- macOS: [deploy/launchd/com.local.pm.plist.example](deploy/launchd/com.local.pm.plist.example)
- Linux: [deploy/systemd/pm.service.example](deploy/systemd/pm.service.example)

替换模板中的二进制、配置文件和工作目录路径后再安装。使用 launchd/systemd 托管时不要加 `-d`，由系统服务负责守护和开机启动。

## 数据文件

默认配置位于 `~/.pm/pm.yaml`，所有默认数据均落在 `~/.pm` 下（自定义配置时相对路径以配置文件所在目录为基准）：

- 配置文件：`~/.pm/pm.yaml`（首次启动自动生成）
- 控制 Socket：`~/.pm/pm.sock`
- 进程日志：由每个 program 的 `stdout_log`、`stderr_log` 决定，示例默认在 `~/.pm/logs/`
- 守护进程日志：后台模式默认 `~/.pm/logs/pm-daemon.log`
- 事件历史：`~/.pm/events.jsonl`（即 `state_dir/events.jsonl`）
- 配置备份：Web 保存配置时生成 `<config>.bak`

PM 不采集或上传遥测数据，Web 静态资源全部内嵌在二进制中。
