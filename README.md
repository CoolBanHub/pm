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

- 进程启动、停止、重启、批量操作和状态查询；进程名和分组（`group:web`、`web:*`）都可作为操作目标
- 配置拆分：主配置通过 `include` 引入 `conf.d/*.yaml` 之类的独立进程文件，支持嵌套、重复检测与来源归属
- 一键迁移 supervisor：`pm import-supervisor /etc/supervisor` 自动转换 program/group 配置
- `pm check` 离线校验配置（含 include 展开），不启动任何进程
- Web 表单新增、编辑和删除进程，无需手写 YAML；include 文件中的进程受保护，不会被误改
- 进程分组、分组筛选、列表全选和选中项批量操作
- `priority` 启动/停止顺序：数字小的先启动、后停止（对齐 supervisor 的 priority）
- `never`、`unexpected`、`always` 自动重启策略；`exit_codes` 自定义预期退出码
- `start_secs` 启动稳定窗口：STARTING 状态，早期退出视为启动失败并重试
- 时间窗重启限流，持续失败后进入 `FATAL`
- 独立进程组管理，优雅停止超时后强制结束整个进程树
- CPU、内存、PID、TCP 监听端口、运行时间、进程树和重启次数监控；Go 程序可选采集 goroutine 数
- stdout/stderr 实时日志与在线日志切割
- 持久化生命周期事件历史，事件文件自动压缩
- 响应式 Web 管理后台和实时日志流
- Web 登录会话、写操作同源校验和仅本机监听的安全默认值
- 严格配置校验、Web 配置编辑、原子替换和自动备份
- 配置 reload；不可热变更的运行参数会明确要求重启守护进程
- 配置值支持 `${VAR}` 环境变量展开（command/args/directory/environment/日志路径）
- 前台运行、后台运行，以及 launchd/systemd 托管模板

## 构建

需要 Go 1.26 或更高版本。

```bash
make build
```

需要管理进程时，可以复制并修改示例配置：`cp pm.example.yaml pm.yaml`，启动时通过 `-config pm.yaml` 显式选用。不创建配置文件也可以先启动 PM。

安装到 `$GOBIN`：

```bash
make install
```

## 启动

前台运行适合调试，或者交给 launchd/systemd 托管：

```bash
./bin/pm daemon
```

未指定 `-config` 时，PM 始终使用家目录默认配置 `~/.pm/pm.yaml`，不会自动读取当前工作目录下的 `pm.yaml`。首次启动若默认配置不存在，PM 会自动创建 `~/.pm/` 并写出一份可编辑的配置。默认配置不含受管进程，使用 `~/.pm/pm.sock`、`~/.pm` 状态目录，并在 `127.0.0.1:19090` 启用 Web。只有明确指定 `-config FILE` 才会使用其他 YAML；指定的文件不存在时会报错。

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

默认使用发起 sudo 的用户的 `~/.pm/pm.yaml`。使用其他配置时必须明确指定：

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

CLI 不依赖 HTTP，所有操作都通过 Unix Socket 完成。CLI 会依次使用 `-socket`、`PM_SOCKET`、`~/.pm/pm.yaml` 中的 `socket`，最后回退到内置的 `~/.pm/pm.sock`。当前目录的 `pm.yaml` 不会改变控制目标：

```bash
./bin/pm status
./bin/pm list
./bin/pm start example-worker
./bin/pm stop example-worker
./bin/pm restart example-worker
./bin/pm start group:workers      # 以分组为目标（等价写法 workers:*）
./bin/pm stop group:workers
./bin/pm reset-restarts example-worker
./bin/pm pause example-worker
./bin/pm resume example-worker
./bin/pm disable example-worker
./bin/pm enable example-worker
./bin/pm logs -n 100 -f example-worker
./bin/pm reload
./bin/pm shutdown
./bin/pm down      # 等价于 pm shutdown
```

上述进程操作都接受多个进程名、`group:分组名`（或 `分组名:*`）或 `all`；`status` 和 `list` 同样支持按组筛选。分组操作按 `priority` 排序执行：启动时数字小的先启动，停止时数字大的先停止。

`restart` 会先重新读取并校验 `pm.yaml`，再用其中的最新进程配置重启目标，因此对 `environment`、命令参数和日志路径的修改会在本次启动生效。配置文件无效时不会停止当前进程；增删进程仍使用 `pm reload`。

`reset-restarts` 清零目标进程的累计重启次数和当前重启限流时间窗，支持多个进程名或 `all`。清零操作不会启动、停止或重启进程，并会写入事件记录。每次自动重启、手动重启或运行参数变更触发的重启都会在事件历史中保存快照，包括时间、触发原因、旧 PID、退出码、错误信息，以及重启前标准输出和标准错误各自最多 8 KiB 的末尾内容；可在 Web 进程详情的“重启记录”中展开查看。快照沿用 `event_history` 的保留数量并持久化在 `state_dir/events.jsonl`。

`pm up` 是 `pm daemon` 的短别名，参数会原样传递，因此 `pm up -d` 等价于 `pm daemon -d`。

`status` 会显示状态、持久化模式、PID、TCP 监听端口、CPU、内存、直接子进程、全部后代进程、Go goroutine、运行时间、启动次数、优先级和退出信息。`list` 是更精简的概览，显示名称、分组、状态、PID、TCP 监听端口、运行时间和描述。

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

默认只监听 `127.0.0.1`，此时账号密码可选（免登录）。只要监听地址超出本机回环地址，配置就必须提供 `username` 和 `password`（两者必须同时配置），否则守护进程拒绝启动：

```yaml
web:
  enabled: true
  listen: 0.0.0.0:19090
  username: admin
  password: "更换为强密码"
```

浏览器访问时使用该账号密码登录；登录后服务端签发有过期时间的会话 token，24 小时无操作自动失效。旧版的 `token` / `token_env` 已废弃，不再作为认证方式，配置了会导致启动报错，请删除并改用账号密码。

局域网访问会以明文 HTTP 传输账号密码。需要跨机器访问时，应在 PM 前放置提供 HTTPS 的反向代理，或使用 SSH 端口转发：

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
| `web.username` | Web 后台登录用户名（须与 `password` 同时配置） | 空 |
| `web.password` | Web 后台登录密码 | 空 |
| `include` | 引入额外进程配置文件的 glob 列表 | `[]` |

### 进程字段

| 字段 | 含义 | 默认值 |
| --- | --- | --- |
| `name` | 唯一进程名，不允许空白字符（支持中文等 Unicode） | 必填 |
| `description` | 进程备注/介绍，不允许换行，至多 256 字符 | 空 |
| `group` | 进程分组，可作为 CLI/Web 操作目标 | `default` |
| `command` | 可执行文件；不会经过 shell 解析 | 必填 |
| `args` | 参数数组 | `[]` |
| `directory` | 工作目录 | 守护进程当前目录 |
| `environment` | 追加或覆盖环境变量 | `{}` |
| `autostart` | 守护进程启动时运行 | `true` |
| `priority` | 启动升序 / 停止降序的排序权重，相同按名称排序 | `999` |
| `restart` | `never`、`unexpected` 或 `always` | `unexpected` |
| `restart_delay` | 自动重启前等待时间 | `1s` |
| `max_restarts` | 时间窗内最大重启次数；`0` 为不限 | `5` |
| `restart_window` | 重启次数统计窗口 | `1m` |
| `exit_codes` | `unexpected` 策略下的预期退出码列表；命中则不重启 | `[0]` 语义（仅 0） |
| `start_secs` | 启动稳定窗口；存活超过该时长才进入 RUNNING，之前退出视为启动失败并重试 | `0s`（关闭） |
| `stop_signal` | `TERM`、`INT`、`QUIT`、`HUP`、`USR1` 或 `USR2` | `TERM` |
| `stop_timeout` | 强制结束前等待时间 | `10s` |
| `stdout_log` | 标准输出日志 | 继承守护进程输出 |
| `stderr_log` | 标准错误日志；可与 stdout 使用同一文件 | 继承守护进程输出 |
| `log_max_bytes` | 单个日志文件切割阈值；`0` 为不切割 | `10485760` |
| `log_backups` | 保留的轮转日志数量 | `3` |
| `pprof_url` | Go pprof 基础地址，用于采集 goroutine 数 | 空（不采集） |

`command` 不经过 shell。需要管道、重定向或循环时，应显式使用 `"command": "/bin/sh"` 和 `"args": ["-c", "..."]`。

`command`、`args`、`directory`、`environment` 值和日志路径支持 `${VAR}` 环境变量展开：读取守护进程自身的环境变量（如 systemd 的 `Environment=`），未定义的变量展开为空字符串。不含 `${}` 的文本（如 shell 片段里的 `$HOME`）原样传递给子进程。

重启语义：`unexpected` 只在退出码不在预期集合内时重启（默认预期集合仅 `0`，可用 `exit_codes` 扩展，如脚本用 `3` 表示正常收尾）；`always` 任何退出都重启；`never` 从不重启。配置了 `start_secs`（默认 `0s` 关闭）时，进程先进入 `STARTING`，存活满该时长才转为 `RUNNING`；在窗口内退出——无论退出码——都视为启动失败并按重启策略重试，秒退的程序因此不会伪装成“正常运行过”。时间窗内重启超过 `max_restarts` 次进入 `FATAL`，`pm reset-restarts` 可清零后重试。`priority` 决定批量启动（升序）和批量停止（降序）的顺序，依赖服务应配置更小的数字。

子进程指标由 PM 从操作系统的 PID/PPID 关系自动采集。TCP 端口仅统计处于 `LISTEN` 状态的 socket，并把主进程及全部后代进程持有的端口去重后展示；Linux 从 `/proc` 读取，macOS 通过系统 `lsof` 读取。操作系统无法仅凭 PID 得知 Go runtime 的 goroutine 数；Go 程序需要引入 `net/http/pprof`、启动仅本机可访问的 HTTP 监听，并配置 pprof 基础地址：

```yaml
pprof_url: http://127.0.0.1:6060/debug/pprof
```

PM 请求该地址下的 `/goroutine?debug=1`，单次采集超时为 750ms。生产环境不要把 pprof 端口直接暴露到公网。

Web 后台可以通过“添加进程”和进程详情中的“编辑配置”完成常用配置，不需要直接编辑 YAML。原始配置编辑器仍保留给高级场景。

后台保存配置时会先写入 `pm.yaml.bak`，再通过同目录临时文件原子替换原配置。进程配置采用差量 reload：未变化的进程保持原 PID，新增、删除或运行参数变化只影响对应进程，单纯修改分组或优先级不会重启进程。Socket、Web 监听、状态目录和事件容量变更需要重启守护进程。

删除进程或修改其运行参数时，PM 会先等旧进程退出（最长 `stop_timeout`）。等待期间其它进程的启停和状态刷新不受影响，同一次 reload 中需要退出的进程会同时停止。若旧进程无法被停止（例如它以其他用户身份运行，PM 没有权限发信号），它会留在列表中并显示最近错误，之后的配置变更不再重复尝试停止它，直到它退出或定义再次被修改。

进程退出后，如果还有继承其输出的子进程在运行（例如程序自己派生的后台进程），PM 最多再等一个 `stop_timeout` 让它们结束。超时后这次运行按已结束处理：仍在该进程组里的进程会被强制结束，这一情况记为该进程的最近错误。进入独立会话的后台进程 PM 管理不到，它不受影响，输出也会继续记录到它退出为止（这类输出流同时最多保留 16 个，超出的会被关闭）。

### 配置拆分（include）

多个应用或部署包共用一个 PM 实例时，可以把进程分散到独立文件，避免共同维护单个 `pm.yaml`：

```yaml
# pm.yaml（主配置）
include:
  - conf.d/*.yaml
```

```yaml
# conf.d/my-app.yaml（drop-in 文件）
programs:
  - name: my-app
    command: /opt/my-app/bin/server
```

规则：

- 主配置与 drop-in 都可以定义 `programs`，按文件名顺序合并；`pm reload` 重新匹配 glob，新增文件自动纳入。
- drop-in 文件只能包含 `programs` 和嵌套 `include`；`socket`、`web`、`state_dir` 等守护进程字段只认主文件，写进 drop-in 会报错。
- 跨文件同名进程直接报错，指明两个来源文件；include 环也会被检测。
- drop-in 中进程的相对路径以该 drop-in 所在目录为基准，conf.d 目录整体搬迁不需要改路径。
- Web 后台的“添加进程”写入主配置；include 文件中的进程在 Web 上编辑或删除会被拒绝，错误信息会指出所属文件，防止改错地方。
- `pm check -config FILE` 会展开 include 后整体校验，适合部署脚本和 CI。

### 从 supervisor 迁移

`pm import-supervisor` 把现有 supervisor 配置转换成 PM 的主配置 + conf.d 结构：

```bash
pm import-supervisor /etc/supervisor -out ~/.pm    # 目录（递归扫描 *.conf/*.ini）
pm import-supervisor /etc/supervisor/supervisord.conf -out ~/.pm   # 单文件
```

转换会生成 `<out>/pm.yaml`（含 `include: conf.d/*.yaml`）和 `<out>/conf.d/<进程名>.yaml`，完成后自动用 PM 的加载器整体校验。字段映射：`autorestart`→`restart`、`startsecs`→`start_secs`、`startretries`→`max_restarts`、`exitcodes`→`exit_codes`、`stopsignal`/`stopwaitsecs`→`stop_signal`/`stop_timeout`、`priority`→`priority`、`[group:x]`→进程的 `group` 字段、`%(ENV_X)s`→`${X}`。目标已存在时拒绝覆盖，加 `-force` 覆盖。`user=`、`umask`、`numprocs`、`[eventlistener:x]` 等 PM 不支持的配置会给出警告并跳过——PM 以发起用户身份运行子进程，需要降权时用 systemd 的 `User=` 托管 PM。

迁移后先 `pm check` 确认，再启动：

```bash
pm check -config ~/.pm/pm.yaml && pm daemon -config ~/.pm/pm.yaml
```

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
