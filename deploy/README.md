# PM 部署指南

本文介绍如何在 Linux 服务器上把 PM 作为系统服务常驻运行。推荐使用 **systemd**（`systemctl`）：它负责后台化、开机自启、崩溃自动拉起和日志收集，是现代 Linux 的标准做法。

> macOS 托管见本文末尾的 [launchd 附录](#macos-launchd-附录)。

## 快速安装

普通单用户 Linux 部署可以直接执行：

```bash
sudo pm systemd
```

该命令安装当前二进制到 `/usr/local/bin/pm`，生成 `/etc/systemd/system/pm.service`，立即启动并设置开机自启。服务以发起 sudo 的原始用户运行，默认使用当前目录的 `pm.yaml`，否则使用该用户的 `~/.pm/pm.yaml`；可通过 `-config FILE` 显式指定。已运行的 PM daemon 会被平滑停止并交接给 systemd。

下文的手动部署适合需要专用 `pm` 系统用户、`/etc`/`/var` FHS 目录、环境文件或额外 systemd 加固的生产环境。

## 核心原则：让 systemd 独占守护职责

PM 自带 `-d` 后台模式（自己 fork 进后台、写 `pm-daemon.log`）。**用 systemd 托管时不要加 `-d`**：

- systemd 自己负责把进程放到后台、监控存活、失败重启、开机启动。
- 再叠加 `-d` 会让 systemd 跟踪到的是 fork 出来的父进程，重启/停止行为会不可预期。
- 守护进程的标准输出/错误由 systemd 接管进 journald，用 `journalctl -u pm` 查看，不再需要 `pm-daemon.log`。

简而言之：**systemd 下 PM 只以前台 `daemon` 模式运行。**

## 1. 准备二进制

PM 是单个静态二进制，仅依赖 Go 标准库和 yaml.v3，无 cgo 依赖。

**在服务器上直接构建**（需要 Go 1.26+）：

```bash
git clone <repo-url> /opt/src/project-manager
cd /opt/src/project-manager
make build              # 产出 bin/pm
```

**或在开发机交叉编译**后上传（推荐，服务器无需装 Go）：

```bash
# macOS / Linux 开发机，目标为 Linux amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o pm -ldflags="-s -w" ./cmd/pm
scp pm user@server:/tmp/pm
# arm64 服务器把 GOARCH 改成 arm64
```

`CGO_ENABLED=0` 产出全静态二进制，可在任意 Linux 发行版上运行。

## 2. 目录与用户规划

采用 FHS 风格布局，并创建专用系统用户（**不要用 root 运行**）：

| 用途 | 路径 | 说明 |
| --- | --- | --- |
| 二进制 | `/usr/local/bin/pm` | 全局可执行 |
| 配置 | `/etc/pm/pm.yaml` | 主配置（用绝对路径） |
| 密钥 | `/etc/pm/pm.env` | 令牌等敏感环境变量，`chmod 600` |
| 运行时 Socket | `/run/pm/pm.sock` | 由 `RuntimeDirectory` 管理，停止即清理 |
| 状态数据 | `/var/lib/pm/` | `events.jsonl` 等，由 `StateDirectory` 管理 |
| 进程日志 | `/var/log/pm/` | 受管程序的 stdout/stderr |

```bash
# 创建专用用户（系统账户，无登录 shell）
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pm

# 目录与权限
sudo mkdir -p /etc/pm /var/lib/pm /var/log/pm
sudo chown -R pm:pm /var/lib/pm /var/log/pm
sudo chmod 750 /var/lib/pm /var/log/pm
# /run/pm 由 systemd 的 RuntimeDirectory 自动创建，无需手动建
```

## 3. 配置文件

部署用的配置**一律使用绝对路径**。配置中的相对路径是相对配置文件所在目录解析的；系统服务下用绝对路径最清晰可控。

`/etc/pm/pm.yaml`：

```yaml
# 控制端 Socket；与 systemd 的 RuntimeDirectory=pm 对应
socket: /run/pm/pm.sock
# 事件历史等运行状态
state_dir: /var/lib/pm
event_history: 1000

web:
  enabled: true
  # 仅监听本机回环；跨机器访问走反向代理（见「安全加固」）
  listen: 127.0.0.1:19090
  # 令牌从环境变量读取，避免明文写进配置
  token_env: PM_WEB_TOKEN

programs:
  - name: my-app
    group: app
    command: /usr/local/bin/my-app
    args: ["serve", "--config", "/etc/my-app/config.toml"]
    directory: /var/lib/my-app
    autostart: true
    restart: unexpected
    stdout_log: /var/log/pm/my-app.log
    stderr_log: /var/log/pm/my-app.error.log
    log_max_bytes: 10485760
    log_backups: 5
```

> 仅监听 `127.0.0.1` 时令牌可选。一旦 `listen` 超出回环（如 `0.0.0.0`），PM 会**强制要求** `token` 或 `token_env`，否则拒绝启动。

令牌放环境变量文件 `/etc/pm/pm.env`（不进配置、不进版本库）：

```bash
sudo tee /etc/pm/pm.env >/dev/null <<'EOF'
PM_WEB_TOKEN=$(openssl rand -hex 24 的实际结果)
EOF
sudo chown root:pm /etc/pm/pm.env
sudo chmod 640 /etc/pm/pm.env
```

> 生成令牌：`openssl rand -hex 24`。把真实值写入文件，不要保留字面的 `$(...)` 占位。

配置文件本身不含密钥，可以放宽权限便于审查：`sudo chmod 644 /etc/pm/pm.yaml`。

## 4. systemd 单元文件

`/etc/systemd/system/pm.service`：

```ini
[Unit]
Description=PM Process Manager
After=network.target

[Service]
Type=simple
User=pm
Group=pm

# 工作目录设为状态目录；受管程序的相对路径以此为基础
WorkingDirectory=/var/lib/pm

# systemd 托管下以前台 daemon 模式运行，不要加 -d
ExecStart=/usr/local/bin/pm daemon -config /etc/pm/pm.yaml

# 受管程序配置支持差量重载
ExecReload=/usr/local/bin/pm -socket /run/pm/pm.sock reload

# 令牌等环境变量
EnvironmentFile=/etc/pm/pm.env

# 运行时目录：自动创建 /run/pm（权限归属 pm:pm），停止时清理
RuntimeDirectory=pm
RuntimeDirectoryMode=0750
# 持久状态目录：自动管理 /var/lib/pm 的所有权
StateDirectory=pm
StateDirectoryMode=0750

# 崩溃自动拉起
Restart=on-failure
RestartSec=3

# PM 守护进程收到 SIGTERM 会优雅停止所有受管程序后再退出；
# control-group 确保退出时整个进程树（含受管程序）一起被回收
KillMode=control-group
KillSignal=SIGTERM
TimeoutStopSec=30

# 优雅停止时给 PM 足够时间回收受管子进程
TimeoutStartSec=10

[Install]
WantedBy=multi-user.target
```

### 单元关键项说明

- `Type=simple` + 前台 `ExecStart`：systemd 直接把该进程视作主进程进行跟踪。
- `Restart=on-failure`：进程异常退出时自动重启；手动 `systemctl stop` 不会触发重启。
- `KillMode=control-group`：PM 会派生受管子进程，按 c组 整体回收，避免停止服务后残留孤儿进程。
- `RuntimeDirectory=pm` / `StateDirectory=pm`：让 systemd 接管 `/run/pm` 与 `/var/lib/pm` 的创建、属主和清理，无需手写 `ExecStartPre`。
- 守护进程的 socket 来自配置文件的 `socket` 字段（`/run/pm/pm.sock`），CLI 控制端需要指向同一 socket（见下文）。

### 可选：进一步加固

PM 会派生并执行**任意外部程序**，受管程序会继承沙箱，因此加固要和你实际运行的程序兼容。以下为常见可加固项，按需启用并充分测试：

```ini
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
# 受管程序需要写入的目录都要放开
ReadWritePaths=/var/lib/pm /var/log/pm /run/pm
# 如不需要网络可限制；但受管程序通常需要网络，按实际情况
# RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
```

## 5. 安装与启用

```bash
# 安装二进制到 PATH
sudo install -m 0755 pm /usr/local/bin/pm     # 或 sudo cp bin/pm /usr/local/bin/pm

# 重载 systemd，识别新单元
sudo systemctl daemon-reload

# 立即启动 + 设置开机自启
sudo systemctl enable --now pm

# 查看状态
sudo systemctl status pm
```

`Active: active (running)` 即正常运行。

## 6. 验证

```bash
# 守护进程日志
sudo journalctl -u pm -n 50 --no-pager

# 通过控制端查询（CLI 默认 socket 是 /tmp/pm.sock，需指向实际 socket）
sudo -u pm /usr/local/bin/pm -socket /run/pm/pm.sock status
```

Web 管理后台（本机）：打开 `http://127.0.0.1:19090`。远程服务器可用 SSH 端口转发访问：

```bash
ssh -L 19090:127.0.0.1:19090 user@server
# 然后本地浏览器访问 http://127.0.0.1:19090
```

### 让控制端默认找到 socket

每次带 `-socket` 较繁琐。给管理员账号配置环境变量即可直接 `pm status`：

```bash
# 写入管理员的 shell 配置（~/.bashrc 或 ~/.zshrc）
export PM_SOCKET=/run/pm/pm.sock
```

> `/run/pm/pm.sock` 由 `pm:pm` 创建，属主可读写。普通管理员账号需有访问权限（可用 `sudo -u pm pm ...` 或调整 socket 目录权限/属组，如把管理员加入 `pm` 组并把 `RuntimeDirectoryMode` 设为 `0750` + 组可读写）。

## 7. 日常运维

```bash
sudo systemctl start pm        # 启动
sudo systemctl stop pm         # 停止（会优雅停止所有受管程序）
sudo systemctl restart pm      # 重启（所有受管程序会随之重启）
sudo systemctl status pm       # 状态
sudo systemctl disable pm      # 取消开机自启

# 实时跟随守护进程日志
sudo journalctl -u pm -f
# 只看今天的错误
sudo journalctl -u pm --since today -p err
```

**受管程序日志**在各自配置的 `stdout_log` / `stderr_log` 文件中（如 `/var/log/pm/my-app.log`），不在 journald 里。可用 `journalctl` 看守护进程自身的输出，用 `tail` / `pm logs` 看受管程序输出：

```bash
sudo -u pm pm -socket /run/pm/pm.sock logs -n 100 -f my-app
```

## 8. 配置变更：reload 还是 restart

| 变更内容 | 操作 |
| --- | --- |
| 仅增删/改受管程序（command、args、重启策略、日志路径、分组等） | `sudo systemctl reload pm`（差量生效，未变的程序保持原 PID） |
| `socket` / `web.*` / `state_dir` / `event_history` | 必须 `sudo systemctl restart pm` |

两种修改途径：

- **Web 后台**：在 `http://127.0.0.1:19090` 编辑/添加进程，保存时自动应用（不可热变更项会提示需要重启守护进程）。
- **直接编辑 `/etc/pm/pm.yaml`** 后执行 `sudo systemctl reload pm`。

```bash
sudo systemctl reload pm
```

`ExecReload` 通过 PM 的控制 Socket 执行差量重载。如果使用旧版单元文件尚未配置 `ExecReload`，可以直接执行：

```bash
sudo -u pm /usr/local/bin/pm -socket /run/pm/pm.sock reload
```

## 9. 升级

```bash
# 1) 拉起新二进制（替换前先停服务更稳妥）
sudo systemctl stop pm
sudo install -m 0755 pm /usr/local/bin/pm
sudo systemctl start pm
```

升级二进制**不需要**改配置或状态；`/var/lib/pm` 与 `/etc/pm` 保持不变，受管程序会随守护进程重启而重启。

## 10. 故障排查

| 现象 | 排查 |
| --- | --- |
| `systemctl status` 显示 failed | `journalctl -u pm -n 100` 看具体错误；常见为配置校验失败或 socket 目录权限问题 |
| 启动报 token 相关错误 | `listen` 超出回环但未配 `token`/`token_env`，或 `PM_WEB_TOKEN` 未注入（检查 `/etc/pm/pm.env` 和 `EnvironmentFile=`） |
| CLI 报 socket 不存在 | 未指向 `/run/pm/pm.sock`，或服务未运行；确认 `pm status` 用了正确 socket |
| `/run/pm` 重启后消失 | 正常，`RuntimeDirectory` 在停止时清理，启动时重建 |
| 受管程序没日志输出 | 检查该程序的 `stdout_log` 路径，及 `pm:pm` 对 `/var/log/pm` 的写权限 |
| 停服后残留子进程 | 确认单元里 `KillMode=control-group`（默认模板已含） |
| 手动改了配置没生效 | 直接编辑文件后需 `pm reload`；改了 socket/web 等需 `systemctl restart pm` |

## macOS launchd 附录

macOS 本机开发或单用户长期运行，建议使用用户级 `LaunchAgent`，不要使用 `sudo`，这样 PM 和受管程序都以当前用户身份运行，配置、日志和工作目录也可以放在用户目录下。模板见 [deploy/launchd/com.local.pm.plist.example](launchd/com.local.pm.plist.example)。要点与 systemd 一致：以前台 `daemon` 模式运行，由 launchd 的 `KeepAlive` 负责守护，不要加 `-d`。

```bash
# 1. 构建并准备配置；把 plist 中所有 /absolute/path/to/* 替换为真实绝对路径
make build
mkdir -p "$HOME/Library/LaunchAgents"
cp deploy/launchd/com.local.pm.plist.example "$HOME/Library/LaunchAgents/com.local.pm.plist"

# 2. 加载（macOS 现代 launchctl 用 bootstrap，不需要 sudo）
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.local.pm.plist"
launchctl enable "gui/$(id -u)/com.local.pm"
launchctl kickstart -k "gui/$(id -u)/com.local.pm"

# 3. 查看和停止
launchctl print "gui/$(id -u)/com.local.pm"
launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.local.pm.plist"
```

如果需要开机后即使没有用户登录也运行，或者在多用户 Mac 上作为系统服务运行，可以改用 `/Library/LaunchDaemons`，并在 plist 中增加合适的 `UserName`，再使用 `sudo launchctl bootstrap system ...`。不要直接用 root 运行 PM，除非受管程序确实需要 root 权限。

launchd 下守护进程的标准输出/错误写到 plist 里指定的 `StandardOutPath` / `StandardErrorPath`（如 `pm-service.log`），不像 systemd 走 journald。
