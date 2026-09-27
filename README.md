# Remote Code Mirror (`rcm`)

面向 AI Remote Development 的 Go 命令行工具：**VPS 是主工作区，Mac 保存真实的本地代码镜像。** IDE 的浏览、搜索和索引不经过网络。没有挂载文件系统、VS Code 插件或云服务。

这是已实现并有自动化测试的 V1。首次使用建议先选一个测试项目。架构决策与边界见 [设计文档](docs/architecture.md)，协议见 [protocol.md](docs/protocol.md)，测试说明见 [testing.md](docs/testing.md)。

## 构建与连接

需要 Go 1.25+；运行时只需要系统 OpenSSH。远端不需要安装 Go、rsync 或 Python。

```sh
make release
# Apple Silicon Mac:
install -m 755 dist/rcm-darwin-arm64 /usr/local/bin/rcm
# Intel Mac 使用 dist/rcm-darwin-amd64

rcm connect eul:/root/projects/myapp
# 另开一个终端：
code ~/Remote/eul/myapp
```

也可以把可执行文件放进自己的 `~/bin`。发布构建包含 Linux amd64 和 arm64 agent；客户端自动探测远端架构、安装到远端 `~/.cache/rcm/` 并启动。`dist/SHA256SUMS` 提供构建产物校验值。所有四种客户端均由同一份代码构建。

`eul` 原样交给系统 `ssh`，继承 `~/.ssh/config` 中的 HostName、User、Port、IdentityFile、ProxyJump 和 ControlMaster 配置。不会关闭 host-key 校验。首次认证建议在前台完成；后台运行需要可用的 SSH agent、密钥或已有复用连接。

```sh
rcm connect eul:/root/projects/myapp \
  --local ~/Remote/eul/myapp \
  --debounce 150ms \
  --background

rcm status
rcm logs myapp
rcm stop myapp
rcm start myapp
rcm remove myapp
```

`connect` 默认前台运行，Ctrl-C 停止；`start` 后台运行。`remove` **仅注销项目，保留镜像、日志和恢复数据**。同名项目用 `--name` 区分；本地镜像目录不能重叠。`logs` 显示最多最后 64 KiB 的日志，`--verbose` 在连接时开启逐路径日志。默认合并输出修改计数。

只编译本机开发版本：

```sh
make build
./dist/rcm connect eul:/root/project --agent-dir ./dist
```

开发版本跨架构连接需要 `--agent-dir` 中存在 `rcm-linux-amd64` / `rcm-linux-arm64`；`make release` 构建内嵌 agent 的完整版本。修改 agent 源码后请重跑 release，避免使用旧的内嵌 agent。远端 macOS 可使用同架构客户端自身，或通过 `--agent-dir` 提供相应 Darwin 二进制。

## 同步与冲突

启动时先监听，再扫描；一次 manifest 和一次 WANT 清单后连续传输差异内容。随后通过持久 SSH 连接推送文件事件，默认 150ms 合并窗口。重命名表示为删除旧路径、创建新路径。新目录会递归注册监听。空目录也同步。

本地修改默认允许上传，但必须基于最后共同版本：

| 共同版本 | 远端 | 本地 | 结果 |
|---|---|---|---|
| A | B | A | 下载 B |
| A | A | C | 上传 C，远端再次核验 A |
| A | B | C | 冲突，保留双方 |
| A | B | B | 更新共同版本，不重复写入 |
| A | 删除 | C | 删除/修改冲突 |

首次连接已有文件的非空目录也执行这个规则；内容不同的同名文件产生冲突。仅本地存在的文件会上传，因此专用空镜像目录是最清晰的初始状态。使用 `--read-only` 可禁止上传，但仍会保护本地修改。

```sh
rcm conflicts
rcm conflict src/foo.go --project myapp
rcm diff src/foo.go --project myapp
rcm resolve src/foo.go --remote --project myapp
rcm resolve src/foo.go --local --project myapp
```

只有一个项目时可省略 `--project`。`diff` 使用系统 `diff` 展示当前本地版本与保存的远端冲突版本。`resolve` 将操作排队交给工作进程；工作进程重新检查版本，有新修改就拒绝过期选择。完成后用 `rcm conflicts` 确认；停止的项目需要先 `start`。V1 不自动合并。

断线自动退避重连，重新比较远端 manifest、本地内容及持久化共同版本，补齐断线期间的增加、修改、删除。本地共同版本每 500ms 合并落盘，初次同步结束、正常退出和冲突时也会落盘；突然断电最多导致保守的额外冲突，不把未确认上传当作成功。连接保活交给 OpenSSH，不进行远程文件轮询。

## 写入与恢复边界

文件内容流式暂存、SHA-256 校验、fsync 后安装。已有文件先通过 rename 移到恢复区，再核验实际移走的版本；新内容使用 **不覆盖目标的原子 rename** 安装。新创建的竞争版本不会被覆盖。目录删除仅使用 rmdir，不能递归删除未知内容或误删刚被换入的普通文件。

这选择了数据保留优先：**替换已有文件时存在短暂的路径不存在窗口**，但不会暴露写了一半的文件。它不等价于传统 `rename(temp, target)` 的无空窗替换。普通文件系统无法对任意外部编辑进程实现按哈希的原子 CAS；持有旧文件描述符的进程仍可能写入被移到恢复区的 inode。这类修改会保留在恢复区，但不能保证自动出现在工作路径。需要最保守的单向模式时使用 `--read-only`，并避免两端同时写同一路径。

两端根目录下的 `.rcm-internal/` 是保留目录，权限 0700，永不通过同步协议传输：

- `history/`：被替换的旧 inode，以及记录原路径、前后哈希和时间的 JSON。`.done` 表示该次处理结束。
- `spool/`：流式接收暂存和持久冲突副本。
- `state.json`：本地共同版本与冲突索引。
- `requests/`：待处理的人工解决请求。

持锁启动时检查未完成的恢复记录；路径仍不存在时恢复旧版本，已有竞争文件时保留它。**V1 不自动清理历史或孤立暂存文件**，避免误删恢复材料；频繁更新大文件会增加磁盘占用。不要在运行中删除这个目录。IDE 可将 `**/.rcm-internal/**` 加到文件/搜索排除；Git 可在项目自己的 ignore 策略中排除它。RCM 不自动改写项目的 `.gitignore`。

## Ignore、Git 和链接

默认忽略：

```text
node_modules/ .venv/ venv/ __pycache__/ dist/ build/ target/
coverage/ .next/ .cache/ *.log .DS_Store
```

规则依次是默认规则、按层级加载的 `.gitignore`、根 `.rcmignore`、配置的 ignore 列表；后面的匹配优先，支持 `!`、`*`、`**`、`?`、字符类、根锚定和目录规则。被忽略的父目录需要先取消忽略才能包含子文件。远端确定实际规则，再传给本地；规则文件更新后自动重连加载。忽略后已存在的镜像文件不会自动删除。

V1 实现常用 Git ignore 语法，**不承诺完整复刻 Git 的所有反斜杠/尾随空格转义语义**。不会查询全局 gitignore 或 `.git/info/exclude`；规则作用于源码，而不是 Git tracked 状态。最终 `.rcmignore` 中的例外不会让已经被父级规则剪枝的目录恢复其内部 `.gitignore`；复杂例外应写在根配置中。

`.git` 默认排除。需要 Source Control / diff 时：

```sh
rcm connect eul:/root/projects/myapp --include-git
```

这是完整 `.git` 的**单向远端镜像**，不是构造一个假的本地历史，也不是不完整的 metadata 子集。大仓库对象数量可能很大；Git 元数据跨多个文件的更新没有事务快照，短暂不一致期间 IDE 可能报错。请把本地 Git 当作只读视图，不在镜像里 commit、checkout、reset、fetch。Git linked worktree/submodule 的 `.git` 指针文件不构成独立可用仓库，V1 不负责重建它们的外部 gitdir。

符号链接同步链接本身，不遍历；绝对目标和词法上逃出根目录、指向内部恢复目录的目标被拒绝并报告。设备、FIFO、socket 等特殊文件不传输。文件保留可执行属性，不同步 owner、ACL、xattr 或原始时间戳。mtime 只用于诊断和不稳定读取检测，内容身份由 SHA-256 确定。

## 配置

本地 `~/.config/rcm/config.yaml`（或 `$XDG_CONFIG_HOME/rcm/config.yaml`）以及**远端根目录**的 `.rcm.yaml`：

```yaml
ignore:
  - data/
  - models/
  - "*.sqlite"
debounce: 150ms
local_write:
  enabled: true
conflict:
  strategy: manual
```

优先级：默认值 → 远端项目配置 → 本地配置/命令行；ignore 按顺序追加，命令行覆盖本地标量配置。`debounce` 范围 10ms–5s。未知配置字段和非 manual 冲突策略会报错。

项目注册表与日志位于 `$XDG_STATE_HOME/rcm/`，默认 `~/.local/state/rcm/`。状态由 OS 锁保护，每个根目录只允许一个工作进程。`stop` 使用私有 Unix socket，不根据可能复用的 PID 发送信号。

Linux 需要足够的 inotify watches；macOS 的 fsnotify/kqueue 会受文件描述符上限影响；进程会尝试在既有硬限制和内核上限内提高软限制。配额失败会显式报错并重试，不静默退化为文件轮询。macOS 运行时、大规模目录的资源限制和真实 WAN 性能仍需要目标机器验证。

```sh
make test
make race
make vet
```
