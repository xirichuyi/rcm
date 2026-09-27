# 验证记录

验证日期：2026-09-28。执行环境：Linux ARM64，Go 1.27.1。项目最低 Go 版本为 1.25。
这是本机文件系统和内存双工连接的测量；不代表真实公网或 macOS 运行时性能。

## 已执行

```sh
go vet ./...
go test -race ./... -count=1 -v -timeout=180s
RCM_SCALE_TEST=1 go test ./internal/mirror -run TestScale20000 -count=1 -v -timeout=120s
./scripts/release.sh
./dist/rcm-linux-arm64 version
```

静态检查通过，完整常规测试与 Go race detector 通过；20,000 文件测试独立执行通过。
四个平台的可执行文件完成交叉编译，并检查了 ELF/Mach-O 的目标架构；Linux ARM64
发布二进制实际运行输出 `rcm 0.1.0 protocol 1`。

原始输出：[race 测试](validation/linux-arm64-race.txt)、[20,000 文件测试](validation/scale-20000.txt)。
大规模测试默认不执行，需显式设置 `RCM_SCALE_TEST=1`。

| 覆盖场景 | 自动化验证 |
|---|---|
| create / modify / delete / rename / mkdir / rmdir | 双工 agent/client 实际文件监听及内容断言 |
| AI burst | 短于一秒内创建 100 文件，每个写三版，验证最终内容和实际逐路径应用次数 |
| 10 / 100 MiB 文件 | 流式传输后检查大小和 SHA-256 |
| 大文件内存 | 100 MiB 接收阶段累计分配约 **0.13 MiB**，测试上限 8 MiB；不是进程总 RSS |
| 断线补齐 | 20 修改、5 删除、10 新建，使用保存的基线重连 |
| 传输中断 | 在大文件正文约 1 MiB 处断开，不出现可见半文件，不推进该文件基线，重连成功 |
| 在线冲突 | 同一共同版本上两端修改，双方原内容保持，生成持久冲突 |
| 冲突解决 | 本地/远端选择、拒绝过期选择、重启后不重现已解决冲突 |
| loop | 远端应用后，逐路径日志中不出现本地回传 |
| 路径边界 | traversal、绝对路径、保留路径、symlink parent/target、损坏状态中的越界 blob |
| 崩溃恢复 | 未完成移动恢复；竞争目标不覆盖；已完成删除不复活 |
| Git / read-only | 普通文件可禁用上传，Git 元数据始终不上传 |
| 协议 | 帧长度、截断、正文校验、版本与消息流程 |
| CLI / SSH 启动 | 实际 CLI 子进程 + SSH 替身：架构探测、agent 安装、引号、缓存、start/stop/remove |
| supervisor 重连 | 终止持久 SSH 替身进程，验证自动重连及离线修改补齐 |

SSH 替身在本机执行真实的远端 shell/agent 命令，但没有执行 SSH 认证、加密、ProxyJump
或实际 WAN 传输。OpenSSH 配置兼容性来源于直接调用系统 ssh；用户真实主机尚未连接。

## 测量

延迟模拟将双向写入分别延迟 150ms，模拟 300ms RTT；不限带宽，不模拟丢包、拥塞或
TCP 重传。配置 debounce=150ms，统计从远端写入完成到本地内容可见：

| 场景 | 结果 |
|---|---:|
| 10 KiB 更新，模拟 300ms RTT | 约 290ms |
| 500 KiB 更新，模拟 300ms RTT | 约 205ms |
| 20,000 小文件 + 100 目录，初次同步（本机无网络延迟） | 61.48s |
| 20,000 文件树，进入 Watching 后首次更新 | 106ms |
| 20,000 文件树，后续稳定更新 | 176ms |

大规模初次同步仍有明显落盘开销，不能宣称已达到 tar/rsync 的吞吐量。普通事件仅重新
读取变更路径/子树；目录事件先合并父子路径，普通文件不遍历整棵树删除前缀。
基线写入合并为每 500ms 一次，避免初次同步对每个文件重写完整 manifest。

代码复用 128 KiB 复制缓冲区。发送用的临时快照不要求持久化；接收文件、冲突副本和
恢复记录需要持久化。已同步过的正文不会在安装阶段无条件重复 fsync。

## 尚未验证

- macOS 上的实际 fsnotify/kqueue 行为、文件描述符上限和 IDE 集成。
- 真实 Debian ARM64 VPS、用户的 SSH 别名和 250–300ms 公网链路。
- 真实断电/磁盘故障；当前恢复测试模拟进程在事务边界中断。
- 大型 Git 对象库和 linked worktree；后者不在 V1 自包含 Git 镜像支持范围内。

仓库提供 Ubuntu/macOS 的 GitHub Actions 工作流，但这里没有执行云端 CI，不把交叉编译
视作 macOS 运行测试通过。
