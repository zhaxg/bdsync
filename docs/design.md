# bdsync — 本地 → 百度网盘 单向备份守护进程

> 目标：把 43,746 文件 / 6,238 目录 / 82.9 GB 的目录树，长期、安静、低资源地备份到百度网盘。
> **第一约束：CPU 与内存占用要小到用户察觉不到，风扇不该因此转起来。**

状态：设计稿 **v2.3** · 2026-10-15
（v1 → v2：新增**镜像模式**（folderRA ≡ folderLA + `.history` 远端历史，默认开启）与
**Web 看板**；Windows 静默化 / 并发限流 / 断点续传已实现并实测；详见 §9.2、§13、§15 记分卡）

v2 → v2.1（2026-10-14）：看板新增**关于**页（功能与核心逻辑摘要，版本号从侧栏与概览
移入该页）；凭据统一经 `baidu.SanitizeCookie` 白名单净化；看板 HTML 响应加
`Cache-Control: no-store`（页面内嵌在二进制里，换版本后刷新就该看到新界面）。

v2.1 → v2.2（2026-10-14）：新增 **§17 运维答疑**，把 5 个高频疑问按实测数据写清
（并发度/首跑序列/休眠恢复/收敛性/队列持久化）；同时**订正三处文档与实现的偏差** ——
休眠失效检测并不存在"上次运行时间戳"、内存无周期性采样告警、DB 无 `integrity_check`
自检（详见 §4.8 表与 §17.3）。

v2.2 → v2.3（2026-10-15）：

- **归档日期桶改用本地日期**。此前取 `time.Now().UTC()`，本地时间 00:00–08:00 之间的删除
  全部落进"昨天"的桶（东八区每天凌晨必现）。归档目标路径、日志、过期清理三处统一时区
  基准 —— 目标与日志若各算一次，跨 UTC 零点会出现"日志说进 A 桶、实际进 B 桶"。
- **补上 §9.2 承诺过的「空目录移除」**。目录从不上传也从不记账：`ClaimBatch` 只取
  `is_dir=0`；`ensureDirs` 只建批内文件的父目录且只记内存 map；`r_fsid` 全代码库只有
  `FinishUpload` 一处写入（文件上传成功路径）—— 于是目录永远没有 `r_fsid`，被
  `HistoryQueue` 的 `r_fsid IS NOT NULL` 挡在门外：文件被 Move 进 `.history` 而父目录留在
  远盘。现于归档阶段结束后自底向上清理变空目录；**删后必须清 `remoteDirs` 缓存**，
  否则后续 `ensureDirs` 会以为目录还在而跳过 Mkdir，上传撞"父目录不存在"。
- **静默期心跳**（`quietHeartbeat = 30min`）。P2 要求无变更时绝对空闲，真正常驻实例的
  日志会长时间没有新行（实测出现过 103 分钟静默段）—— 这在设计上是正确的，但只看日志
  无法区分"安静地活着"和"卡死/被杀"，两者长得一模一样。折中：只在 QUIET / PAUSED 时
  每 30 分钟打一行，忙时一行不多打。RSS 用 `processMem()`（工作集，不触发 GC），
  不用 `sampleMem()`（会 `runtime.GC()`，与 P2 冲突）。
  ⚠ 已知局限：远端对账仍会补建**本地就存在的空目录**（`mirror.go` 补建段，
  `LocalManifest` 不区分有无文件后代），实测本地有 965 个空目录。此条待定，见 §17.6。

---

## 0. 三条设计原则

| 原则 | 含义 | 由此产生的具体决策 |
|---|---|---|
| **P1 · 检测与发送彻底解耦** | 检测只碰本地元数据，绝不因网络阻塞 | 探测器 / 持久队列 / 发送器三段独立，中间用 SQLite 承接 |
| **P2 · 无变更时绝对空闲** | 没有活干时不消耗任何 CPU/IO | USN Journal 作为主检测后端 + 电源状态机 + Windows 优先级控制 |
| **P3 · 状态可持久化** | 批处理不产生数据丢失风险 | 队列落 SQLite，检测结果即刻持久化；崩溃/断电不丢已检测到的变更 |
| **P4 · 只支持本地固定磁盘** ★ | 网络盘上无法可靠检测变更 | 启动时强制校验；连带砍掉两个有损后端（§1.1） |

P3 是 P1 能够成立的前提：因为队列在本地磁盘上，"每 5 分钟才发一次" 这个策略的代价只是「文件还没上云」，而不是「变更被丢弃」。

---

## 1. 目标与非目标

### 目标

- 单向：本地 → 网盘。**镜像模式（v2，默认开启）**：folderRA 主树的路径清单永远 ≡ folderLA
  （含删除与改名的传播）；所有会被镜像操作销毁的远端旧版本先归档进 `folderRA/.history/`（保留 90 天）。
  关闭 `mirror.enabled` 可回退 v1 纯备份语义（不传播删除）——见 §9.2
- 增量：稳态下只处理真实变更，不重扫、不重算哈希、不重传
- 低噪声：空闲态 CPU ≈ 0%，RSS < 30 MB，磁盘不休眠唤醒
- 长驻：连续运行数周不 OOM、不泄漏、不因系统状态变化（休眠/时间跳变/网络断开/日志盘满）而崩溃
- 可观测但低开销：能回答"我的数据有多安全"，但不为此常驻高 CPU
- 跨平台：Windows（NTFS）与 Linux（本地 ext4/xfs/btrfs）共用同一套核心逻辑

### 非目标（明确不做）

- ❌ **网络盘 / 网络共享** ★ —— 见 §1.1
- ❌ 双向同步 / 冲突合并 —— 复杂度高 60%，备份场景不需要
- ❌ 裸删除（不归档就删远端）—— v2 镜像模式下删除/覆盖必须先归档进 `.history`（§9.2）；
  `mirror.enabled=false` 时回退 v1 语义：远端删除完全不传播
- ❌ 图形界面 —— 用 SSE Web 看板 + CLI，零 GUI 依赖（避免把 syncnorris 那种 Wayland/X11 依赖树引进来）
- ❌ 内置 Web 服务对外暴露 —— 只监听 `127.0.0.1`，默认关闭

### 1.1 为什么砍掉网络盘（这是最重要的一个范围决策）

**决定：只支持本地固定磁盘，启动时校验，不符合则拒绝运行。**

这个砍除连带消掉了两个技术方案：

| 被砍掉的东西 | 原本为谁而存在 |
|---|---|
| `ReadDirectoryChangesW` 后端 | 它的**唯一**优势场景是网络共享（USN 不可用时的替代） |
| `inotify` 后端 | 它的**主要**价值是网络挂载（stat 慢到几十秒）与超高频写入 |

砍掉之后，检测层退化成只有两个实现，且两个都是"不会丢事件"的：

```
Windows 本地 NTFS   →  usn    （无损 · 可验证）
所有其他本地文件系统 →  poll   （无损 · 天然完整）
```

**这是纯粹的设计简化，没有引入任何新问题。** 代价是：如果将来用户有 NAS 上的数据要备份，需要另做设计（那时才引入 lossy 后端 + 强制对账机制，见 §4.5）。

#### 代价说明（如果以后需要支持）

- 网络盘上 `stat` 慢 10–100 倍，43,746 文件的 poll 扫描从 1–3 秒变成 20–120 秒
- 那时 poll 不再可行，必须用有损机制 → **必须同时实现「全量对账」兜底**，否则会静默丢文件
- Windows 侧还要处理 UNC 路径、`FILE_SHARE_*` 协商、`\\?\UNC\` 前缀
- Linux 侧要处理 `ESTALE`（NFS 句柄失效）、`max_user_watches` 配额（内核默认 8192 < 6,238 目录的余量需求）

**明确记下来，免得将来当成"漏做"而不是"有意排除"。**

#### 启动时的强制校验

不能靠文档约定，要在代码里拦住：

```go
// Windows
//   GetDriveTypeW(root) 必须 == DRIVE_FIXED (3)
//   DRIVE_REMOTE(4)=网络盘  DRIVE_REMOVABLE(2)=U盘  均拒绝
//   还要拒绝 UNC 路径（strings.HasPrefix(path, `\\`)）

// Linux
//   解析 /proc/self/mountinfo，取最长前缀匹配
//   fstype ∈ {nfs, nfs4, cifs, smb3, 9p, fuse.sshfs, fuse.rclone, ceph, glusterfs}
//   → 拒绝并给出明确提示
```

拒绝时必须输出**可操作的错误信息**，而不是"不支持"：

```
✗ 不支持网络路径：D:\Backup on \\NAS\share
  本工具仅支持本地固定磁盘。原因：网络盘上文件变动无法可靠检测，
  可能导致备份静默遗漏文件。
  如需备份 NAS 数据，请先将数据同步到本地磁盘再使用本工具。
```

#### 这个决策的连带收益

- 检测层实现数从 3 降到 2，代码量减少约 40%
- 不需要处理 `IN_Q_OVERFLOW` / `ERROR_NOTIFY_ENUM_DIR` / 改名配对丢失等一整类问题
- 不需要调试内核配额（`max_user_watches`）
- **USN 成为唯一的高级检测后端**，可以把它做深、做对，而不是三个都做浅

---

## 2. 容量分析（基于实测反推）

用户实测：SVIP 账号，命令行跑完全量无报错，约 30 分钟。

| 推算项 | 数值 | 结论 |
|---|---|---|
| 吞吐 | 82.9 GB ÷ 1800 s ≈ **46 MB/s** | SVIP 级带宽 |
| 文件速率 | 43,746 ÷ 1800 ≈ **24 文件/秒** | |
| 推算 API 速率（小文件 ≈ 3 请求：precreate + 1 片 + create） | **≈ 70–100 req/s** | |

**判定：瓶颈是带宽，不是 API 限流。**

推论（直接影响设计）：

1. 限流器起步就给足（并发 12–16），让 AIMD 向上探，而非从保守值爬升
2. 优化重点是 **不浪费带宽**，不是省请求数 —— 少传无谓的字节比少发请求值钱得多
3. 增量同步会很快。日常量级大概率是几十 MB / 几百文件，**秒级到分钟级**
4. 5 分钟批处理不是为了扛吞吐，是为了摊薄扫描开销 + 提供清晰的备份检查点语义

### 朴素实现的成本（为什么它会吵）

朴素方案 = 每分钟 `filepath.WalkDir` 43,746 个文件。真实成本不是 CPU：

| 存储介质 | 43,746 次 `GetFileInformationByEx` | 主要开销来源 | 范围 |
|---|---|---|---|
| **NVMe SSD（本地）** | 1–2 s 墙钟，~400 ms CPU | 系统调用开销 | ✅ 目标场景 |
| SATA SSD（本地） | 2–5 s 墙钟 | 系统调用 + 队列深度 | ✅ |
| 机械硬盘（本地） | 2–10 s 墙钟 | **磁盘唤醒 + 寻道** | ⚠️ 可用但吵 |
| SMB / NAS 共享 | 20–120 s 墙钟 | 每次 stat 一个网络 RTT | ❌ 已排除（§1.1） |
| 任意介质 + Defender 实时防护 | 上述 ×5–20 | **每个文件打开触发 AV 扫描** | ★ 头号杀手 |

**三个真实杀手**（前两个是物理成本，第三个是配置成本）：

1. **Defender 实时扫描**：这是"我的同步工具把风扇转起来"的头号原因。43,746 次文件打开，每次触发一次杀毒扫描，CPU 和 I/O 同时放大一个数量级。
2. **磁盘唤醒**：机械盘即使只读元数据也会被唤醒，空闲功耗从 5 W 跳到 15 W+，**可闻**。
3. **SMB 延迟**：不是 CPU 忙，是海量 `syscall` 等待，但会让 CPU 占用曲线看起来像在满载 —— 这也是排除网络盘的理由之一。

§4 的全部设计就是针对前两点；第三点靠范围排除规避。

---

## 3. 架构总览

```
┌──────────────── Detector ────────────────┐
│  Windows NTFS : USN Journal（无损·可验证）│
│  其他本地文件系统 : WalkDir 全量扫描        │
│         ↓ 仅变更项                         │
│  与索引 diff (size, mtime_ns)               │
└──────────────────┬─────────────────────────┘
                   │ 写入（零网络 I/O）
                   ↓
        ┌────── SQLite (WAL) ──────┐
        │  entry 表 + pending 状态  │  ← 崩溃安全边界
        │  kv 表 (USN 游标/状态)    │
        └──────────┬───────────────┘
                   │ 批量领取（原子事务）
                   ↓
        ┌────────── Sender ─────────────────┐
        │  预建目录 → 秒传探测 → 分片上传    │
        │  AIMD 限流 → 断点续传 → 回写索引   │
        └──────────┬───────────────────────┘
                   ↓
              百度网盘 API
```

三条原则的落点：

- **P1**：Detector 与 Sender 之间只有 SQLite，没有任何内存队列或 channel
- **P2**：§4 整章
- **P3**：Detector 写完即持久化，Sender 只读不假设

### 组件职责边界

| 组件 | 允许做 | 禁止做 |
|---|---|---|
| Detector | 读本地元数据、写 SQLite | 任何网络调用、任何文件内容读取 |
| Sender | 网络调用、写远端、更新 SQLite | 遍历目录树（除 bootstrap） |
| Web 看板 | 读 SQLite、写日志 | 任何写操作 |

Detector 零网络是硬性约束 —— 它保证了「检测耗时」与「网络状况」完全解耦，扫描耗时永远可控。

---

## 4. 静默设计 ★

### 4.1 检测后端选型

排除网络盘后，**只有两个后端**，且两者都不会丢事件：

| 后端 | 适用条件 | 空闲期 CPU | 磁盘 I/O | 丢失性质 |
|---|---|---|---|---|
| **usn** ★ 主选 | Windows 本地 NTFS | ≈0（只读内核已缓存的 journal） | **0**（不打开任何文件） | 无损 · **丢失可检测** |
| **poll** 兜底 / Linux 主选 | 所有本地文件系统 | 每次 ~400 ms CPU（1–3 s 墙钟） | 全量元数据读 | **天然完整**（不存在丢失这回事） |

**关键性质：两个后端都是"可证明完整"的。** 这消除了整个设计中最危险的一类问题 —— 备份工具静默漏掉一个文件。

#### 为什么 Windows 上 usn 是必须的

在 NTFS 上，usn 在空闲时：

- 只做一次 `DeviceIoControl(FSCTL_READ_USN_JOURNAL)`，数据来自 NTFS 内核已缓存的 journal
- **不打开任何文件** → 不唤醒磁盘，不触发 Defender
- 成本与目录大小无关，只与变更数量有关
- 即使有 43,746 个文件、83 GB，空闲时也是**零 I/O、零磁盘唤醒**

对比 poll：每次扫描 43,746 次 `GetFileInformationByEx`，在本地 SSD 上约 1–2 秒墙钟 / 400ms CPU。5 分钟一次 ≈ 0.13% CPU，**也不算吵，但会产生真实的磁盘 I/O**（在机械盘上可闻）。

**结论：Windows 用 usn 换掉这 1–2 秒的磁盘活动，值。**

#### 为什么 Linux 上 poll 是默认

Linux 没有 USN 的等价物（见 §4.10），inotify 的主要价值是网络挂载和超高频写入 —— **前者已排除，后者不适用于本场景**。

本地 ext4/xfs 上 `WalkDir` 43,746 文件只要 **1–3 秒**，每 5 分钟一次 ≈ **1% CPU**，和 inotify 阻塞等待的 CPU 占用基本没区别。但 poll 有 inotify 没有的：

- **零正确性风险** —— 全量遍历天生不会漏
- **无需处理任何失效场景** —— 没有游标、没有回绕、没有队列溢出、没有 watch 失效、目录改名后 watch 跟丢
- **代码量约 1/3**

> **选"要么最快、要么最稳"的，不要选中间那个。**

自动探测顺序：启动时校验根目录是本地固定磁盘 → 尝试打开卷 `\\?\C:` 并读取 `FSCTL_USN_READ_JOURNAL` → 成功则选 usn；否则选 poll。

### 4.2 USN Journal 原理

NTFS 为每个卷维护一个追加写的变更日志（USN Journal）。任何文件创建/修改/删除/改名都会往里追加一条记录。

```
时间轴：
  t0: 游标 = 12000（持久化在 kv 表）
  t1: 用户改了 a.txt        → journal 写入 USN 记录 12008
  t2: 用户建了 b/c.txt      → journal 写入 USN 记录 12040
  t3: 我们的守护进程醒来    → 一次 ioctl 读出 12008..12040 两条记录
```

一次系统调用拿到期间所有变更，与目录规模无关。

#### 关键实现要点

**a) 两级结构**

```
FSCTL_USN_READ_JOURNAL   → USN_JOURNAL_DATA_V2 { UsnJournalID, FirstUsn, NextUsn,
                                                        LowestValidUsn, MaxSize }
   ↓ 用它校验游标有效性，并判断是否回绕
FSCTL_READ_USN_JOURNAL   → READ_USN_JOURNAL_DATA_V0 { StartUsn, ... } + USN_RECORD_V2[]
   ↓ 真正的变更记录
```

**b) 记录格式（USN_RECORD_V2）**

```
RecordLength                4
MajorVersion/MinorVersion   2+2
FileReferenceNumber         8   ← 本文件
ParentFileReferenceNumber   8   ← 父目录
Usn                         8   ← 单调递增
TimeStamp                   8
Reason                      4   ← 变更原因位图
SourceInfo/SecurityId       4+4
FileAttributes              4
FileNameLength              2
FileNameOffset              2
FileName                    N   ← UTF-16LE，文件名（不含路径）
```

需要关心的 Reason 位：

| 常量 | 值 | 处理 |
|---|---|---|
| `USN_REASON_DATA_OVERWRITE` | 0x00000002 | 入队 |
| `USN_REASON_DATA_EXTEND` | 0x00000001 | 入队 |
| `USN_REASON_FILE_CREATE` | 0x00000100 | 入队 |
| `USN_REASON_RENAME_NEW_NAME` | 0x00002000 | 入队（改名后的新名） |
| `USN_REASON_FILE_DELETE` | 0x00000200 | **不入队**，仅标记本地已删 |

**c) 路径解析 —— 最麻烦的一环**

USN 记录只有「父目录 FileReferenceNumber + 文件名」，**没有完整路径**。必须自己拼。

策略：**构建期建立引用号 → 路径映射表**。

```sql
CREATE TABLE dir_ref (
  ref      INTEGER PRIMARY KEY,   -- NTFS FileReferenceNumber
  rel_path TEXT NOT NULL          -- 相对 root 的路径
);
```

bootstrap 全量扫描时，同时记录每个目录的 MFT 引用号。之后：

```
USN 记录 (parentRef=0x1A2B, "photo.jpg")
  → 查 dir_ref[0x1A2B] = "assets/2024"
  → 完整相对路径 = "assets/2024/photo.jpg"
```

目录改名时：`RENAME_OLD_NAME` + `RENAME_NEW_NAME` 成对出现，据此更新 `dir_ref` 里该 ref 的 rel_path，并给子树整体打上「路径待解析」标记（子目录的 rel_path 需重写）。实践中目录改名是低频事件，可以懒处理：只在实际需要时递归修正。

退化方案（不想维护映射表时）：用 `NtQueryDirectoryFile` 配合 `FileIdBothDirectoryInformation` 逐级向上查父目录名。正确但慢，仅在解析失败时兜底。

**d) 三个必须处理的坑**

| 坑 | 检测方式 | 处理 |
|---|---|---|
| **日志回绕** | `NextUsn - LowestValidUsn` 大于我们的游标到 `NextUsn` 的距离 | 我们的记录已被覆盖 → **产生空洞** → 回退全量扫描，告警 |
| **Journal ID 变化** | 每次读取比对 `UsnJournalID` | NTFS 修复/格式化/换盘后 ID 会变 → 旧游标全部作废 → 全量扫描 |
| **休眠唤醒后失效** | 同上，靠 `cursor < LowestValidUsn` 自身判定 | 机器关机数天后 journal 必然回绕 → 唤醒时必定走全量扫描 |

**长期运行的守护进程必须把「游标失效 → 全量扫描」当作正常路径，而不是异常。** 这不是 bug。

**e) 权限**

`CreateFile("\\\\?\\C:", GENERIC_READ, FILE_SHARE_* , ..., OPEN_EXISTING, 0, 0)` 对普通用户可用（只需读权限，不需要 `SeManageVolumePrivilege`，后者是写 journal 才需要）。若被策略/容器拒绝，探测阶段捕获并退到 poll。

### 4.3 为什么不做 `ReadDirectoryChangesW`（决策记录）

这一节保留下来，不是为了实现，而是为了**避免将来有人重新提出这个方案**。

RDCW 常被当作 Windows 的"文件变动事件"推荐方案。但对备份工具，它有一个致命缺陷：**它是天生的有损机制**。

核心区分：

> **USN 是「可验证的完整」；RDCW 是「不可验证的不完整」。**
>
> 对备份工具，静默漏掉一个文件 = 一次你永远不会知道的数据丢失。

#### 四个具体失败模式

**① 内核缓冲区有上限，突发写入必然溢出**

`FILE_NOTIFY_INFORMATION` 每条 = 12 字节头 + 文件名（2 字节/字符）。20 字符的文件名约 52 字节。

| 缓冲区 | 可容纳事件数 |
|---|---|
| 64 KB | ~1,260 |
| 1 MB | ~20,000 |

一次 `go build` 或 `npm install` 改 5,000 个文件，64KB 缓冲区**必然溢出**。溢出后内核直接丢弃后续事件。

**② 溢出信号的语义很别扭**

fsnotify（Go 的标准封装）源码：

```go
case windows.ERROR_MORE_DATA:
    // The i/o succeeded but the buffer is full.
    // In theory we should be building up a full packet.
    // In practice we can get away with just carrying on.
    n = uint32(unsafe.Sizeof(watch.buf))     // ← 直接当正常处理，不报错
```

```go
if n == 0 {
    w.sendError(ErrEventOverflow)           // ← 只有这里才报告溢出
}
```

上游作者自己都不确定 `ERROR_MORE_DATA` 路径是否安全，只是实测没炸 —— **这是"碰巧没出事"，不是"设计上不会出事"**。

**③ 改名配对是单槽状态，跨界就丢**

```go
case windows.FILE_ACTION_RENAMED_OLD_NAME:
    watch.rename = name            // ← 单个 string，不是 map/队列
```

`watch.rename` 是**一个槽位**。两个改名交错，或 OLD_NAME 与 NEW_NAME 落在两次不同的缓冲区填充里：

- 槽位被覆盖 → 配错对
- 只有 OLD_NAME → 上游**根本不发出事件**（只有 `RENAME_NEW_NAME` 才 emit）→ 这次改名完全看不见

漏一次改名 = 旧文件留在网盘 + 新文件没传 = 两份陈旧数据。

**④ 默认掩码不含 SIZE**

fsnotify 只注册 `FILE_NOTIFY_CHANGE_LAST_WRITE` 和 `FILE_NOTIFY_CHANGE_FILE_NAME|DIR_NAME`，没有 `FILE_NOTIFY_CHANGE_SIZE` / `CREATION` / `ATTRIBUTES`。

#### 结论

RDCW 唯一无可替代的场景是**网络共享**（USN 不可用时的替代）。排除网络盘（§1.1）后，USN 在所有 Windows 场景都严格优于 RDCW —— 更快、更完整、无需任何兜底逻辑。

> **若将来必须支持网络盘**，才需要引入 RDCW/inotify 这类有损机制，并且**必须同时实现「周期性全量对账」兜底**（§4.5）。没有兜底的有损机制对备份工具是不可接受的。

### 4.4 电源状态机

核心思路：**平时压到最低，被唤醒时短暂拉高**。不是持续低优先级，而是「空闲时几乎没有活动」。

```
        ┌──────────────────────────────────────────────┐
        │  QUIET（默认，绝大部分时间）                   │
        │                                              │
        │  · 进程优先级 = IDLE_PRIORITY_CLASS           │
        │  · Job Object CPU 硬上限 = 5%                │
        │  · usn: 5m 轮询一次 USN 游标                 │
        │  · poll: 不轮询，直接睡到下一个 scan_interval │
        │  · 无 dashboard 推送                          │
        │  · 进入时调用 debug.FreeOSMemory()            │
        │                                              │
        │  实测：CPU < 0.1%，RSS ≈ 20 MB，磁盘不休眠      │
        └──────────────┬───────────────────────────────┘
                       │ USN 报告变更 / poll 定时到期
                       ↓
        ┌──────────────────────────────────────────────┐
        │  ACTIVE                                      │
        │                                              │
        │  · 进程优先级 = NORMAL_PRIORITY_CLASS        │
        │  · CPU 硬上限 = 80%（留 20% 给前台应用）     │
        │  · usn: 10s 轮询一次                          │
        │  · 执行 detect → enqueue → (到点则) drain    │
        │                                              │
        │  实测：CPU 峰值 60%（哈希阶段），网络等待期 < 5%│
        └──────────────┬───────────────────────────────┘
                       │ 队列排空 + 静默 idle_threshold(5m)
                       ↓
                    回到 QUIET
```

两个容易忽略的细节：

1. **状态切换要有滞回**（hysteresis）。没有滞回的话，一个每 30 秒被改一次的文件会让进程在 ACTIVE/QUIET 之间反复横跳，每次切换都要 `SetPriorityClass` + `FreeOSMemory`，反而更吵。
2. **`FreeOSMemory()` 只在进入 QUIET 时调用**，不是定时调。它会强制一次完整 GC + 归还内存给 OS，成本不低但绝对值很小。

### 4.5 Windows 资源控制

#### 优先级（最有效的单一手段）

```go
func setPriority(active bool) {
    cls := uint32(windows.IDLE_PRIORITY_CLASS)   // 0x40
    if active { cls = windows.NORMAL_PRIORITY_CLASS }  // 0x20
    windows.SetPriorityClass(windows.CurrentProcess(), cls)
}
```

在 16 核机器上，`NORMAL` 优先级的后台任务会与前台应用争抢；`IDLE` 则让位给一切。配合「只在有活时提升」，这是让程序真正安静下来的关键。

> 注意：优先级不能设成 `REALTIME`，永远不要。

#### Job Object CPU 硬上限

```go
// 创建 job 并把当前进程放进去
jo, err := windows.CreateJobObject(nil, nil)
windows.AssignProcessToJobObject(jo, windows.CurrentProcess())

// 设置 CPU 速率上限（CpuRate 单位是 1/100 百分比：10000 = 100%）
rate := windows.JOBOBJECT_CPU_RATE_CONTROL_INFORMATION{
    ControlFlags: windows.JOB_OBJECT_CPU_RATE_CONTROL_ENABLE |
                  windows.JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP,
    CpuRate:      1500,   // 15%
}
windows.SetInformationJobObject(
    jo, windows.JobObjectCpuRateControlInformation,
    uintptr(unsafe.Pointer(&rate)), unsafe.Sizeof(rate))
```

**权衡**：HARD_CAP 是节流而非拦截。上限 15% 时哈希变慢 → 上传也变慢。所以：

- QUIET 态：硬上限 5%（此时没在传东西，无副作用）
- ACTIVE 态：放宽到 80%，或改用 `WEIGHT` 模式（按权重分享而非硬性截断）

先测你的实际上限，再决定。多数场景下**光靠优先级就够了**，CPU cap 是保险丝。

#### 内存回收

```go
// 进入 QUIET 时
runtime.GC()
debug.FreeOSMemory()                     // 归还内存给 OS
windows.SetProcessWorkingSetSize(
    windows.CurrentProcess(), ^uintptr(0), ^uintptr(0))  // 修剪工作集
```

#### 电源节流（Win10 1709+）

```go
// 需 lazy DLL 加载，golang.org/x/sys/windows 未覆盖
// 建议：进程启动时设为 VERY_LOW，仅在 ACTIVE 时放开
procSetProcessInformation.Call(
    windows.CurrentProcess(), 4 /*ProcessPowerThrottling*/,
    uintptr(unsafe.Pointer(&throttling)), unsafe.Sizeof(throttling))
```

> 具体常量值请在实现时核对 SDK 头文件（`PROCESS_POWER_THROTTLING_EXECUTION_SPEED` 系列）。此项优先级最低，先不做也能达到目标。

### 4.6 Go 运行时控制

```go
// 启动时（cmd/bdsync/main.go）
debug.SetGCPercent(400)        // GC 不那么频繁（默认 100）
debug.SetMemoryLimit(256 << 20) // 软内存上限 256MB（Go 1.19+）
```

要点：

- **别用 `SetGCPercent(-1)` 完全关 GC**。守护进程跑几周，一个泄漏就能 OOM。用高 GC 阈值 + 显式 GC 是更安全的组合
- **预分配所有大 slice**。`make([]T, 0, len(dst))` 避免反复扩容触发 GC
- **buffer 池用 256 KB 而不是 4 MB**。秒传需要算 4MB 分块的 md5，但可以流式过 256KB 缓冲区，不需要整块驻留
  - 16 并发 × 256KB = **4 MB**（若用 4MB buffer 就是 64 MB，差了 16 倍）
- **绝不要 `time.Tick`**（会泄漏）。一律 `time.NewTicker` + `defer Stop()`
- 生产构建加 `GOAMD64=v3`（若 CPU 支持），实测可省 10–20% CPU

### 4.7 资源预算表

| 项目 | 预算 | 说明 |
|---|---|---|
| Go 运行时基线 | ~8 MB | |
| SQLite page cache | 8 MB | `PRAGMA cache_size=-8000` |
| 索引（usn 模式） | <1 MB | **不常驻全表**，见下 |
| 索引（poll 模式） | 12–16 MB | 43,746 × ~300B |
| pending 队列 | <1 MB | 日常几百条 |
| hash/上传 buffer 池 | 4 MB | 16 × 256KB |
| HTTP (SSE) | ~5 MB | |
| **空闲态 RSS 合计** | **≈ 25–30 MB** | |
| **活跃态 RSS 峰值** | **≈ 50 MB** | |
| **空闲态 CPU** | **< 0.1%** | |
| **活跃态 CPU 峰值** | **60–70%** | 仅哈希阶段，网络等待期 < 5% |

**usn 模式内存小的原因**：全量索引不需要常驻内存。Detector 拿到的 USN 记录只包含"真正变了的路径"，对每条做一次 SQLite 点查即可判断是否已入队。**查询次数 ∝ 变更数，与总文件数无关。**

poll 模式确实需要全量（遍历本身就是全量的），但可以用紧凑定长记录代替 `map[string]*struct` 减轻 GC 压力：

```go
// 紧凑索引：定长记录 + 二分查找，替代 map
type entryRec struct {
    size    int64
    mtimeNs int64
    hash    uint64   // 路径的 FNV-1a
}
const entryRecSize = 24
// 43,746 × 24B = 1.05 MB，且无 GC 压力（存 []byte 里）
```

### 4.8 长期运行的稳定性清单

守护进程要跑几周，下面每一条都会咬人：

| 风险 | 检测 | 处理 |
|---|---|---|
| USN journal 回绕 | `Usn > NextUsn` 或游标 < `LowestValidUsn` | 全量扫描 |
| USN Journal ID 变化 | 记录到 `kv` 表，每次比对 | 全量扫描 |
| 休眠/休眠唤醒 | 同回绕检测（`cursor < LowestValidUsn`）；另有启动时无条件全量扫描兜底 | 全量扫描 |
| 内存泄漏 | 每次看板请求读 `runtime.MemStats` 展示 RSS/存活堆（仅观测） | 定期 `debug.FreeOSMemory()` |
| 日志撑爆磁盘 | 日志大小上限 + 轮转（保留 7 份 × 10MB） | 循环删除最旧 |
| 系统时间跳变 | 变更判定用**文件自身 mtime**，不依赖墙上时钟 | `settle` 用相对时间 |
| 网络断开 | Sender 指数退避到上限后进入「挂起态」，Detector 继续 | 指数退避封顶 30 分钟 |
| DB 损坏 | **未实现自动检测**（WAL + 事务保证原子性；损坏需人工介入） | 从备份恢复 / 重建索引 |
| 盘符变化/目录被删 | 每次 ACTIVE 前校验 root 存在 | 报错但守护进程存活 |
| SQLite WAL 膨胀 | 长事务是主因；Detector 每 N 条提交一次 | 定期 `PRAGMA wal_checkpoint(TRUNCATE)` |

### 4.9 跨平台检测抽象

排除网络盘后虽然只有两个后端，**仍然要做成可插拔接口**。理由不是"以后可能要加 inotify"，而是：

1. `usn_windows.go` 里的 Windows 专属 API 调用必须能被编译器隔离，否则交叉编译 Linux 版会挂
2. 有明确的对照基准 —— poll 是 usn 的正确性对照组，出现不一致时立刻能定位是检测层还是比对层的 bug
3. `SelfTest` 机制可以统一在接口层强制

```go
// internal/detector/detector.go  （全平台，无 build tag）

type Kind uint8
const (
    KindCreated Kind = iota
    KindModified
    KindDeleted
    KindRenamed
)

type Event struct {
    Kind    Kind
    Path    string   // 相对 root，统一 '/'
    OldPath string   // 仅 KindRenamed
}

// Backend 只负责「告诉调用方哪些路径值得看一眼」。
// 它不做任何比对决策 —— 判断始终由上层读索引完成。
type Backend interface {
    Name() string

    // Start 开始监控。初始快照不归它管，由调用方先跑一次 FullScan。
    Start(ctx context.Context, root string) error

    // Events 返回变更流。可能返回 nil channel（poll 后端由定时器驱动）。
    Events() <-chan Event

    // SelfTest 写一个探测文件并确认收到了对应事件。
    // 10s 内没收到就返回错误。这是防止"调用成功但从不通知"的唯一手段。
    SelfTest(ctx context.Context, root string) error

    // Stats 后端健康指标，进 bdsync status
    Stats() Stats

    Close() error
}
```

实现文件：

```
internal/detector/
  detector.go           -- 接口 + Event + 共用类型
  poll.go               -- 全平台，portable
  usn_windows.go        -- //go:build windows
  localpath_windows.go  -- //go:build windows  （GetDriveTypeW 校验）
  localpath_linux.go    -- //go:build linux    （mountinfo 校验）
  selftest.go           -- 共用的探测文件逻辑
```

#### 三条硬规矩

**① `SelfTest()` 是接口必实现方法，不是可选的。**

启动时跑一次，失败就降级 + 告警。虽然排除网络盘后两个后端都不太可能静默失效（poll 是主动扫描，usn 有 journal ID 校验），但**机制本身要有** —— 它是唯一能发现"环境变了"的手段（比如目录被移到了 BitLocker 加密卷、卷被换掉、企业策略变更）。

```go
func (b *Backend) SelfTest(ctx context.Context, root string) error {
    probe := filepath.Join(root, fmt.Sprintf(".bdsync-probe-%s.tmp", uuid.NewString()))
    if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
        return err
    }
    defer os.Remove(probe)

    // 等事件通道里出现 probe，最多 10s
    // poll 后端：直接验证「下一轮扫描能看到它」
}
```

**② 判定逻辑永远读索引，不读事件流。**

事件只是"这个路径值得看一眼"的提示。拿到路径后一律走 `stat + 与索引比对`。这样任何后端都只是性能优化，**不可能引入正确性 bug**。

**③ 全量扫描必须永远可用。**

哪怕 Windows 上默认 usn，`poll` 实现也不能删 —— 它既是降级路径，也是 `SelfTest` 的对照基准。**没有降级路径的守护进程不是好守护进程。**

#### 统一的全量对账触发条件

**任何后端都不能被当作唯一真相来源。**

```
触发全量对账 =
    后端主动要求（USN 游标失效 / Journal ID 变化）
  OR SelfTest 失败
  OR 周期到达（reconcile_interval，默认 24h）
  OR 检测到休眠 / 关机唤醒
```

按后端的丢失倾向设不同周期：

| 后端 | 默认对账周期 | 理由 |
|---|---|---|
| `poll` | 24h | 天生完整，纯粹防索引损坏 / 目录消失 / 后端环境变化 |
| `usn` | 24h | 同上 + 防 journal 异常 |

自适应调整：若近期发生过游标失效，临时把周期压到 1h，连续 3 天无异常后恢复默认。**自适应比固定值可靠。**

### 4.10 Linux 支持

#### 没有 USN 的等价物

Linux 上的候选机制评估：

| 机制 | 能拿到路径吗 | 会不会丢 | 需要 root | 结论 |
|---|---|---|---|---|
| **inotify** | 能（目录级 + 文件名） | 会（`IN_Q_OVERFLOW`） | 否 | 唯一现实选项 |
| **fanotify** | **不能**（modify 事件无路径） | 会 | 是 | 只给"某挂载点有变化"的布尔值，对同步无用 |
| **eBPF 钩 `vfs_write`** | 能（PID + 路径） | 会（ring buffer 丢） | 是 | 为备份工具重写一套内核态采集 + 持久游标，**杀鸡用牛刀** |

**Linux 上不存在持久化、可回溯、可校验的变更日志。** 这不是实现问题，是内核设计如此。

#### 所以 Linux 默认 poll

| | Windows NTFS | Linux 本地 SSD |
|---|---|---|
| 默认后端 | usn（无损 · 快） | **poll（无损 · 简单）** |
| 原因 | Windows stat 慢 + Defender 放大，轮询代价高 | ext4/xfs 上 `WalkDir` 43,746 文件只要 **1–3 秒** |

算一下：3 秒 / 300 秒 = **1% CPU**。这和 inotify 阻塞等待的 CPU 占用基本没区别，但 poll 零正确性风险、代码量少 2/3。

**inotify 保留为"已评估、暂不实现"的扩展点。** 值得做的唯一场景是：**数据在本地机械盘上**（stat 会唤醒磁盘，1–3 秒变 2–10 秒且可闻）。届时在 `poll` 之上叠加 inotify 是纯增量的，不影响现有代码。

> 记录一下 inotify 的关键限制，以便将来真的需要时不必重新调研：
> `fs.inotify.max_user_watches` **内核默认仅 8192**，而本项目有 **6,238 个目录**，占掉 76%。systemd / Debian / Ubuntu 通常通过 `sysctl.d` 调到 524288，但最小化容器里可能仍是 8192。必须用**单个** inotify 实例挂全部 watch（`max_user_instances` 默认仅 128）。内核内存约 1KB/watch × 6,238 ≈ 6 MB，可忽略。

#### Linux 专项坑

| 问题 | 处理 |
|---|---|
| **`ETXTBSY`**（文件正在执行） | Linux 版 sharing violation，与 Windows `ERROR_SHARING_VIOLATION` 同等处理：defer，不计失败 |
| **`ESTALE`**（NFS） | 已排除网络盘，但 bind-mount / 容器挂载仍可能出现。**不要当错误**，重新 stat |
| **`ENOTDIR` / `ELOOP`**（符号链接环） | poll 用 `filepath.WalkDir` 默认**不跟随符号链接**，天然规避；若开了 `--follow-links` 必须用 `(dev, ino)` 集合防环 |
| **大小写敏感** | Linux ext4 **大小写敏感**，与网盘一致；与 Windows 行为相反（见 §10）。索引里的路径必须原样保留大小写 |
| **非法文件名字符** | Linux 允许的字符比 Windows 多，但**网盘**的限制两边都要满足。校验逻辑必须与 Windows 端共用同一份规则表 |
| **xattr / ACL / SELinux 标签** | **备份不了** —— 目标是百度网盘，只存字节。见 §11.1 的范围声明 |
| **稀疏文件** | `SEEK_HOLE`/`SEEK_DATA` 可检测，平均 1.9MB 的场景不值得做 |
| **ZFS / Btrfs** | 若数据在这两个文件系统上，**用原生快照做备份远优于文件级同步**，值得在文档里提示用户 |
| **时区** | 本地文件 mtime 按 UTC 存储，上传到网盘后按 UTC 解释。**不要在任何环节做时区转换** |

#### systemd 集成

```ini
# /etc/systemd/system/bdsync.service
[Unit]
Description=bdsync — 百度网盘备份守护进程
After=network-online.target

[Service]
Type=notify                      # 配合 sd_notify(READY=1)
NotifyAccess=main
ExecStart=/usr/local/bin/bdsync daemon
Restart=on-failure
RestartSec=10
# 静默强化
Nice=10
IOSchedulingClass=idle
CPUQuota=80%
MemoryMax=128M

[Install]
WantedBy=multi-user.target
```

- `Type=notify` + `sd_notify(READY=1)` 让 `systemctl status` 和启动顺序正确工作
- `IOSchedulingClass=idle` 是 Linux 版的「IDLE 优先级」，对应 Windows 的 `IDLE_PRIORITY_CLASS`
- `MemoryMax=128M` 给硬上限（与 Go 的 `SetMemoryLimit(256MB)` 配合，后者更早触发 GC）
- 用 `systemd-run --on-calendar` 做定时模式时，记得配 `Persistent=true`（补跑错过的周期）

---

## 5. 数据模型

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA cache_size   = -8000;      -- 8 MB
PRAGMA foreign_keys = OFF;
PRAGMA busy_timeout = 5000;

CREATE TABLE sync_pair (
  id            INTEGER PRIMARY KEY,
  local_root    TEXT    NOT NULL,
  remote_root   TEXT    NOT NULL,
  account       TEXT    NOT NULL,
  detector_mode TEXT    NOT NULL,   -- usn | notify | poll
  last_flush    INTEGER,
  last_verify   INTEGER,
  schema_ver    INTEGER NOT NULL
);

CREATE TABLE entry (
  pair_id       INTEGER NOT NULL,
  rel_path      TEXT    NOT NULL,     -- 统一 '/' 分隔（跨平台 + 网盘原生）
  is_dir        INTEGER NOT NULL,
  scan_run      INTEGER NOT NULL,     -- 最近一次扫描中被访问到的 run id

  l_size        INTEGER,
  l_mtime_ns    INTEGER,             -- ★ 快速判定主力
  l_md5         TEXT,                -- 懒计算，持久化
  l_settled_ns  INTEGER,             -- 最后一次观察到变更的时间

  r_fsid        INTEGER,
  r_size        INTEGER,
  r_mtime       INTEGER,
  r_md5         TEXT,

  state         INTEGER NOT NULL,
  renamed_from  TEXT,                -- 改名前路径（镜像 R 阶段远端 mv 用；NULL=无改名）
  upload_id     TEXT,                -- 分片断点续传
  done_blocks   TEXT,                -- 已完成 block 列表(JSON)
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_err      TEXT,
  last_sync     INTEGER,
  PRIMARY KEY (pair_id, rel_path)
) WITHOUT ROWID;

-- ★ 部分索引：Sender 只看未完成的 4 万条 IDENTICAL 不占空间
CREATE INDEX idx_pending ON entry(pair_id, state, l_mtime_ns DESC)
  WHERE state NOT IN (1, 9);

-- usn 模式的路径解析表
CREATE TABLE dir_ref (
  pair_id  INTEGER NOT NULL,
  ref      INTEGER NOT NULL,          -- NTFS FileReferenceNumber
  rel_path TEXT    NOT NULL,
  PRIMARY KEY (pair_id, ref)
) WITHOUT ROWID;

-- 杂项状态：usn 游标、journal id、历史清理时间戳（hist_prune_at）、各种时间戳
CREATE TABLE kv (k TEXT PRIMARY KEY, v BLOB);
```

### state 枚举

| 值 | 名称 | 含义 |
|---|---|---|
| 0 | `UNKNOWN` | 见过但未定论 |
| 1 | `IDENTICAL` | 两端一致 ✓ |
| 2 | `LOCAL_NEW` | 本地有、网盘无 → 待传 |
| 3 | `REMOTE_NEW` | 网盘有、本地无（备份场景保留，仅记录） |
| 4 | `LOCAL_MOD` | 本地已变更 → 待传 |
| 5 | `CONFLICT` | 预留（双向才用） |
| 6 | `ERROR` | 上次失败，可重试 |
| 7 | `IN_PROGRESS` | 正在传 |
| 8 | `UNSTABLE` | 正在被写入，settle 未通过 |
| 9 | `REMOVED_LOCAL` | 本地已删 —— **远端数据保留** |

---

## 6. 检测流程

### 6.1 usn 模式（主路径）

```
唤醒（5m 定时 或 notify 事件）
  │
  ├─ 校验 USN Journal ID 与 kv 记录一致？  ──否──► 全量扫描
  ├─ 校验游标 ≥ LowestValidUsn？            ──否──► 全量扫描（有空洞）
  │
  └─ FSCTL_READ_USN_JOURNAL(cursor → NextUsn)
        │
        ├─ 遍历 USN_RECORD_V2
        │    ├─ Reason & FILE_DELETE      → 标 state=REMOVED_LOCAL，不入队
        │    ├─ Reason & RENAME_OLD_NAME  → 记入改名配对表
        │    ├─ Reason & RENAME_NEW_NAME  → 标记改名，配对消解
        │    ├─ isDir(FileAttributes)     → 目录：更新 dir_ref，必要时递归修正子树
        │    └─ 文件变更                   → resolvePath(parentRef, name)
        │                                     → 点查 entry
        │                                       ├─ 不存在 → INSERT, state=LOCAL_NEW
        │                                       └─ 已存在且 (size,mtime_ns) 变了 → UPDATE, state=LOCAL_MOD
        │
        ├─ 持久化新游标（每 N 条或每轮结束一次）
        └─ 有新增/变更 → 切 ACTIVE
```

**每轮只做「点查」不做「全扫」**，这是 usn 模式内存与 CPU 都低的根源。

### 6.2 poll 模式（Linux 主选 / Windows 兜底）

```go
func (d *Detector) ScanAll(ctx context.Context) error {
    runID := d.nextRunID()
    var batch []Change

    err := filepath.WalkDir(d.root, func(p string, de fs.DirEntry, err error) error {
        if err != nil {
            if de != nil && de.IsDir() { return fs.SkipDir }
            return nil                      // 无权限目录跳过，不中断
        }
        if ctx.Err() != nil { return ctx.Err() }

        rel, _ := filepath.Rel(d.root, p)
        rel = filepath.ToSlash(rel)          // 索引统一 '/'

        if de.IsDir() {
            d.upsertDir(ctx, rel, runID)
            return nil
        }
        if d.exclude.Match(rel) { return nil }

        info, err := de.Info()
        if err != nil { return nil }
        size, mtime := info.Size(), info.ModTime().UnixNano()

        changed, err := d.index.IsChanged(ctx, rel, size, mtime)
        if err != nil || !changed { return nil }

        batch = append(batch, Change{rel, size, mtime})
        // ★ 分批落库，避免超长事务撑大 WAL
        if len(batch) >= 2000 {
            if err := d.index.CommitBatch(ctx, batch, runID); err != nil { return err }
            batch = batch[:0]
        }
        return nil
    })
    if err != nil { return err }
    if len(batch) > 0 { d.index.CommitBatch(ctx, batch, runID) }

    return d.index.MarkMissing(ctx, runID)   // 本轮未见 = 本地已删
}
```

`scan_run` 机制很关键：只有 `entry.scan_run == runID` 的记录才被视为"仍存在"，其余标记 `REMOVED_LOCAL`。这样无需在内存里维护全量路径集合。

---

## 7. 发送流程

镜像模式（v2，默认开启）下，Drain 依次执行四个阶段（均幂等、可中断续跑）：

```
① prune   历史过期清理（kv 每日一次守卫）：.history/<日期> 超过 history_days → 整桶删除
② R 改名  ClaimRenamed（目录优先）→ 远端 mv；成功后按本地实况撤回 IDENTICAL 或留待传
          （文件逐个比对 l_size/l_mtime；目录整棵 mv 后逐子文件确认）
③ H 归档  state=REMOVED_LOCAL 且远端有副本 → Move → .history（源=renamed_from 或现路径）
④ 上传    见下（uploadOne 前置：覆盖前先归档旧版）
```

```
drain(ctx):
  loop:
    pending_bytes = query()
    if pending_bytes == 0:                    → 返回
    if now - last_drain < min_gap:            → 返回（本轮已 drain 够久了）

    batch = ClaimBatch(n=cfg.BatchSize)       -- 原子事务，state → IN_PROGRESS
    if len(batch) == 0:                       → 返回

    ensureDirs(batch)                         -- 按深度排序，父先于子，批量建
    runConcurrently(batch, cfg.Concurrency, uploadOne)
    updateFlushTime()
```

### uploadOne

```
0. 镜像前置（mirror on 且远端同路径有副本 r_fsid）
   → 先归档旧版（Move → .history）；归档失败绝不覆盖（历史优先）
   → 0 字节文件：跳过秒传/分片，单请求直传（服务端对空内容的秒传/合并会报 code 2）
1. 稳定性闸门
   info = os.Stat(full)
   if info.Size() != e.l_size || info.ModTime().UnixNano() != e.l_mtime_ns:
        → state=UNSTABLE, defer（本轮不算失败）
   if now - e.l_settled_ns < cfg.SettleDuration:   → state=UNSTABLE, defer
   if ERROR_SHARING_VIOLATION:                     → defer（不算失败）

2. 计算秒传标识
   sliceMD5     = md5(前 256KB)
   contentMD5   = md5(整个文件)
   blockListMD5 = 每 4MB 分块的 md5
   → 流式 256KB buffer，不整块驻留

3. RapidUpload 预提交
   ├─ IsRapidUpload == true
   │     → 秒传完成，零字节。state=IDENTICAL, r_* 回填。记秒传命中
   └─ false
         → 得到 UploadID + 缺失 block 列表
            ★ 持久化 upload_id + done_blocks（崩溃可续）

4. 逐个上传缺失 block
   UploadTmpFile(uploadID, path, blockSeq, offset, uploadFunc)
     ← 每片独立重试：指数退避 + full jitter
     ← 成功后立刻更新 done_blocks（保证中途崩溃不丢进度）

5. UploadCreateSuperFile(uploadID, policy, size, path, checksumMap)

6. 回写
   state=IDENTICAL, r_fsid, r_size, r_mtime, last_sync=now
   失败 → state=ERROR, attempts++, last_err
```

### 并发模型

```go
sem := make(chan struct{}, cfg.Concurrency)
var wg sync.WaitGroup
for _, t := range batch {
    wg.Add(1)
    sem <- struct{}{}
    go func(t Task) {
        defer wg.Done()
        defer func() { <-sem }()
        uploadOne(ctx, t)
    }(t)
}
wg.Wait()
```

上传器是**网络密集**的，CPU 在等 I/O 时接近 0。真正吃 CPU 的是第 2 步的哈希，所以 CPU 上限设在 ACTIVE 态是安全的。

---

## 8. 调度：水位 + 间隔

不是纯定时，是双触发：

```
触发 drain = (距上次 drain 结束 ≥ flush_interval)  OR  (pending_bytes ≥ flush_bytes)
```

```
   pending_bytes
        │
  8GB ──┤                                      ┌─────────────────
        │                    ┌─────────────────┘
  2GB ──┤─── high_water ─────┤  ← 立即触发，不等间隔
        │        ╭───────────╯
512MB ──┤─── low_water ───╯   ← drain 退出点
        │  ╲
   0 ───┴───╲────────────────────────────────────────
        空闲  │            时间 →      │
               └── flush_interval ─────┘
```

**drain 内部是 work-conserving 的**：一旦启动就持续排空到低水位，而不是「5 分钟内传完」。否则 43,746 文件的首次同步会变成每 5 分钟猛冲一次然后被限流打断。

实现要点：
- `min_gap`（默认 30s）：drain 结束后至少间隔这么久才允许下一轮，避免连续两轮把网络打满
- ticker 触发时若已在 drain，直接合并（不排队）
- `flush_interval` 从 **drain 结束**时开始计时，不是从开始

---

## 9. 正确性

### 9.1 稳定性闸门（Windows 上尤其重要）

大量软件会写文件：下载中的视频、数据库、Office 临时文件、`~$foo.docx`。**正在被写的文件传上去就是坏数据。**

```sql
-- 首次检测到变更时记录
l_size = 12345, l_mtime_ns = 1730000000000000000, l_settled_ns = now
```

发送前重新 `stat`，比对 `(size, mtime_ns)`：变了 → 说明还在写 → `state=UNSTABLE`，退回队列，**不计为错误**。加上 `settle_duration`（默认 10s）双保险。

5 分钟的批处理间隔天然提供了 5 分钟静默期，这是免费的，但显式闸门不能省 —— 因为 `flush_interval` 可配置，用户可能调到 30 秒。

### 9.2 删除与覆盖传播：镜像 + `.history` 远端历史（v2，默认开启）

v1 的"绝不传播删除"升级为**镜像模式**（`mirror.enabled` 默认 true，可关闭回退 v1）：

> **folderRA 主树的路径清单永远 ≡ folderLA 的清单（被跟踪路径）；
> 任何会被镜像操作销毁的远端旧版本，先归档进 `.history` —— 归档 = Move，
> 一次服务器端调用同时完成"历史写入 + 主树删除"，零带宽。**

| 本地事件 | 远端动作 |
|---|---|
| 新增 | 上传（秒传优先）——不变 |
| 修改 | ① 旧版 `Move → .history/<UTC日期>/<路径>@<远端mtime>` ② 上传新版 |
| 删除 | 旧版 `Move → .history/...`（主树即被清空，无第二步）；空目录移除 |
| 改名 | **远端直接 mv**（零历史污染零带宽）；poll 后端收不到改名事件时降级"归档+重传" |
| 清单外远端项（其他客户端写入等） | **归档而非删除**（决策 Q2，附录 B）；整棵未知子树一次 mv 归档 |

关键设计：

- **归档命名确定性**：`.history/<YYYY-MM-DD>/<路径>@<远端mtime秒>` → 同名不撞、
  **幂等三步**（Meta 目标已存在 = 上次已成功 → 补账；Move；源 31066 = 已处理 → 跳过），崩溃可重入
- **保留期**：`mirror.history_days`（默认 90），每日清理一次（kv `hist_prune_at` 守卫），
  过期**整日期桶**服务器端递归删除；非日期命名的目录永不自动删
- **毒丸防护**：归档失败绝不改行状态（尤其不 FailUpload —— 那会把"本地已删"翻成待传重传）；
  单轮不重复尝试同一条目，下轮自动重试
- **首跑安全网**：`send --dry-run` 完整列出"将归档 N 项（含清单）+ 覆盖前需归档 N 项"；
  daemon 启动即做本地对账与远端清单对账（清单外 → 归档、缺失目录 → 补建）
- **误归档恢复预案**（已实测固化于 recovery 真机测试）：内容仍在服务端 →
  全部重新入队 → 秒传命中，**零带宽重建主树**。曾用于一次清单判断 bug 引发的
  主树全量误归档事故（`LocalManifest` 的值语义 isDir 与存在性判断混用 → 所有文件被误判清单外）

v1 语义回退：`mirror.enabled: false` → 本地删除只标 REMOVED_LOCAL，远端原样保留。

清理远端历史 → `bdsync reconcile --prune`（可 `--dry-run` 预览）。

### 9.3 幂等与崩溃安全

| 中断点 | 恢复后行为 |
|---|---|
| Detector 写了一半 | WAL 事务原子，回滚到批次边界 |
| 秒传预提交后崩溃 | 下次重跑预提交（幂等，最多多算一次哈希） |
| 部分 block 已传 | `upload_id + done_blocks` 持久化，只传缺失的 |
| `UploadCreate` 前崩溃 | 已传 block 是服务端孤儿，不影响正确性，重跑预提交即可 |
| 回写索引前崩溃 | 下次检测到 `(size, mtime)` 仍与 `l_*` 一致但 `state≠IDENTICAL` → 重新入队（**宁可多传一次，不漏传**） |

最后一条是设计原则：**所有不确定情况一律偏向「重传」而非「跳过」**。多传浪费一次带宽，跳过是数据丢失。

---

## 10. Windows 专项坑

| 问题 | 现象 | 处理 |
|---|---|---|
| **文件名非法字符** `\ / : * ? " < > \|` | 传到一半失败，前面几千文件白传 | **扫描阶段拦截**，记 `path_map`，或直接报错让用户先处理 |
| **保留名** `CON PRN AUX NUL COM1-9 LPT1-9`（含 `COM¹²³` 上标变体） | 创建失败 | 加前缀转义 |
| **结尾的 `.` 和空格** | Windows 本身不允许，但外部归档可能有 | 规范化时去除 |
| **路径 > 260 字符** | 部分 API 失败 | 用 `\\?\` 前缀 |
| **大小写** ★ | NTFS 默认大小写不敏感，**网盘敏感**。`A.txt`→`a.txt` 本地是 no-op 但远端是变更 | 比对必须**大小写敏感**（用 `==` 不用 `EqualFold`）；纯大小写改名识别为 rename |
| **文件被占用** | `ERROR_SHARING_VIOLATION` | **不算错误**，defer 到下轮 |
| **Defender 实时扫描** | 哈希阶段 CPU/I/O 放大 5–20× | **把同步目录加入 Defender 排除项**（见 §11），这是最有效的单点优化 |
| **OneDrive 同步目录** | OneDrive 自行改 mtime、占句柄 | 不要放在 OneDrive 目录下 |
| **休眠 / 关机** | USN 游标失效 | 唤醒后预判走全量扫描（§4.8） |
| **网络盘 / UNC 路径** | 检测不可靠 | **启动时拒绝**，`GetDriveTypeW != DRIVE_FIXED` 或路径以 `\\` 开头即报错退出（§1.1） |
| **可移动盘（U盘）** | 可能随时拔出，且非 NTFS | `DRIVE_REMOVABLE` 一并拒绝 |

**跨平台一致性提醒**：文件名合法性校验规则表（非法字符、保留名、结尾点空格）必须与 Linux 端**共用同一份实现**。虽然 Linux 本地允许的字符更多，但**目标网盘**的限制两边都要满足 —— 校验的是网盘的规则，不是本地文件系统的规则。

建议放在 `internal/netdiskname/` 下的纯函数，Windows 和 Linux 都 import 它。

---

## 11. 部署与调优

### 首次安装必做的一件事

```
把备份目录加入 Windows Defender 排除项
```

PowerShell（管理员）：

```powershell
Add-MpPreference -ExclusionPath "D:\BackupRoot"
```

这一条能同时降低 CPU、降低磁盘 I/O、缩短扫描时间，是投入产出比最高的调优。**做完之后再看性能数据。**

### 常驻方式

优先用 **Windows 任务计划程序**（免写服务代码）：

```
触发器: 登录时 / 系统启动时
设置:   不论用户是否登录都运行
        "如果任务正在运行，则不启动新实例"
        失败后每 1 分钟重启，无限次
```

若要真正的服务语义（无用户会话、SCM 集成、自动重启），用 `golang.org/x/sys/windows/svc` 实现服务，注册为 `auto start`。

### 优先级策略

- **不要设 REALTIME，永远不要**
- 活跃期 `NORMAL` 或 `ABOVE_NORMAL`（用户主动触发的备份，值得优先）
- 空闲期 `IDLE`

### 11.1 明确的范围声明（写进 README 和 `bdsync --help`）

工具**只备份文件内容 + 修改时间**，不保存：

| 不保存 | 原因 |
|---|---|
| 文件权限 / ACL | 目标网盘无此概念 |
| xattr / SELinux 标签 / ADS | 同上 |
| 符号链接本身 | 只备份链接目标指向的内容（且需 `--follow-links` 显式开启） |
| 硬链接关系 | 多条路径各自独立备份，内容可能重复上传 |
| ACL / 文件所有权 | 同权限 |

**这不是缺陷，是目标存储的固有限制。** 但必须显式声明 —— 否则用户会在某天发现权限丢失时觉得是 bug。

需要保留权限的场景，这个工具不合适；应该用 `restic` / `borg` 这类面向块存储的备份工具。

### 11.2 Linux 部署

```bash
install -m755 bdsync /usr/local/bin/bdsync
```

见 §4.10 的 systemd unit 文件。首次运行前同样要做"排除扫描器"的对等操作 —— Linux 上对应的是：

```bash
# 如果用了 clamav 之类的实时扫描器，把备份目录加进白名单
# ClamAV: 在 clamd.conf 里配置
#   ExcludedDirectory /data/backup
```

大多数 Linux 桌面/服务器默认没有实时全盘扫描，这一项通常不需要做。

---

## 12. 错误分类与自适应限流

SDK 返回的是 `pcserror.Error`（接口），**不是 `error`**，直接 `errors.As` 会失败，需先接口断言。

```go
type Action int
const (
    RetryNow Action = iota  // 网络抖动
    RetryBackoff            // 限流 / 远端 5xx
    DegradeToUpload         // 31079 秒传失败 → 走普通分片上传
    SkipFile                // 31066 文件不存在
    Fatal                   // 31045 登录过期 → 立即停机告警
)

func Classify(e pcserror.Error) Action
```

| 码 | 含义 | 策略 |
|---|---|---|
| `31045` | 登录状态过期 | **Fatal**，停止并通知用户重新登录 |
| `31061` | 文件已存在 | 按 policy 处理 |
| `31066` | 文件/目录不存在 | `SkipFile` |
| `31079` | 秒传失败 | `DegradeToUpload`（不是错误！） |
| `errno 6` | 请求过于频繁 | AIMD 降并发 + 退避 |
| `ErrTypeNetError` | 网络错误 | `RetryBackoff` |

**AIMD 自适应并发**（起点给足，SVIP 实测无感）：

```
起始 12 → 连续 N 次成功 → +1（每 30s 最多 +1，防抖）
        → 遇限流      → ×0.5，立即退避
退避: sleep = rand(0, min(30s, base × 2^attempt))   ← full jitter，必须有
```

没有 jitter，16 个 worker 同时重试就是惊群，能把账号打死。

---

## 13. 可观测性（低开销）

### 核心指标

```
最老待传文件年龄        ★ 数据丢失敞口，唯一真正重要的数字
秒传命中率              ★ 效率 KPI
限流次数（按 errno）
已传字节 / 已传文件数 / 失败数 / 队列深度
```

### 零成本输出

```
$ bdsync status
  上次备份      2 分钟前
  队列          17 文件 / 84.3 MB
  最老待传      3 分钟          ← 数据丢失敞口
  本次秒传命中  92% (15/16)
  进程 RSS      26 MB
  电源状态      QUIET
```

### Web 看板（v2 已实现）

SSE，`127.0.0.1` 监听，**默认关闭**（`--web` 或 `web.enabled` 开启；`web.addr` 强制校验回环地址）。
除回环校验外，还有请求来源校验 + Origin 同源校验（防跨站调用），以及
`Cache-Control: no-store`（HTML 内嵌在二进制里，不让浏览器缓存糊弄版本）。

**已实现能力**（Windows 11 风格单页，零外部依赖、明暗双主题）。四个页面由 hash 路由
`#/overview` `#/config` `#/logs` `#/about` 切换，URL 即状态，刷新与前进后退都对得上；
旧链接 `#/control` 仍兼容（自动落到 `#/overview`）：

- **概览**：状态机（空闲/活跃/暂停）、队列（待传/最老等待/推迟/待归档）、内存、上次扫描/排空；
  运行控制（暂停/继续/停止）已并入本页 —— 原「运行控制」页已撤销，其信息并到概览底部两张卡
- **配置**：基本表单（检测/发送间隔、settle、并发、静默阈值、排除规则）+ 高级 YAML 全文编辑；
  保存 = 校验 → 热更新子集立即生效（间隔/并发/settle/策略/排除）→ 结构性字段（mode/路径/web）
  只写盘并明确提示"需重启"；配置文件带安全校验（`propagate_delete=true` 拒绝启动等）；
  另含**网盘凭据**区：粘贴新 cookie 即可就地换绑（过期时不必重走向导），并可清除已保存的凭据
- **日志**：SSE 实时流 + 环形缓冲 500 行断点续传（`?after=`）、15s 心跳、自动重连退避
- **关于**：功能清单与核心逻辑摘要（检测→SQLite→发送的数据流、四条设计原则、稳定性闸门 /
  水位调度 / AIMD / 错误分类等关键机制、明确的非目标），是本文档 §0/§1/§3/§4/§8/§9/§12 的精简版；
  **版本号只在此页展示**（侧栏底部与概览页均已移除）

**凭据净化**（`baidu.SanitizeCookie`）：浏览器整行复制出的 cookie 有几十项，与上传无关的占
绝大多数，其中还不乏长期有效的标识（PSTM、RT、BDCLND…）；且百度的 RT 带引号，`net/http`
每次带它出门都会刷一行 `invalid byte '"' in Cookie.Value`。因此所有凭据入口 —— 看板粘贴、
环境变量、`credentials.dat` 读回 —— 统一过一遍白名单：只保留 `BDUSS`/`STOKEN`/`BAIDUID`，
并逐字节剔除 RFC 6265 `cookie-octet` 之外的字符。旧文件里存的整串 cookie 也自愈，
不必等用户重粘。**注意**：`31079` 秒传失败、`31066` 文件不存在这类**不是**净化能解决的问题，
仍按 §12 的错误分类处理。

**低开销要点**（维持 v1 约束）：

- 推送频率 **1 Hz 上限**，绝不 60fps；日志事件即时投递，状态由客户端收到事件后节流拉取
- **无客户端连接时完全不推送**（最常见的浪费）
- 心跳 15s 一次，仅在有连接时
- 服务端不做任何周期性数据库轮询 —— 状态快照按请求构建（内存字段 + 单次索引查询）

> 绝不引入 Grafana/InfluxDB 这类常驻组件。实测看板内存占用 < 5 MB。

---

## 14. 配置

```yaml
pair:                     # ★ 必填：备份对（缺任一项 → 拒绝启动）
  local_root: "D:/YourData"         # 本地根目录（严格只读）
  remote: "/bdsync-backup"         # 网盘内路径，必须以 / 开头

detector:
  mode: auto            # auto | usn | poll
                          #   auto  → Windows NTFS 自动选 usn，其余选 poll
                          #   usn   → 强制（仅 Windows 本地 NTFS）
                          #   poll  → 强制全量扫描
  poll_interval: 5m     # poll 模式的扫描间隔
  usn_poll_quiet: 5m    # usn 模式 QUIET 态的游标轮询间隔
  usn_poll_active: 10s  # usn 模式 ACTIVE 态的轮询间隔
  reconcile_interval: 24h   # ★ 强制全量对账周期（兜底，后端失效/索引损坏的保险）
  selftest_interval: 1h     # 后端自检周期（已实现为显式 `bdsync selftest` 命令）

sender:
  flush_interval: 5m    # ★ 最小发送间隔（从上次 drain 结束计时）
  flush_bytes: 2GB      # 高水位：队列超此立即触发
  min_gap: 30s          # 两轮 drain 之间的最小间隔
  concurrency: 12       # 起步给足，AIMD 上下探（遇限流 ×0.5）
  settle_duration: 10s  # 稳定性闸门：变更后须静默这么久才上传
  policy: overwrite     # overwrite | skip | rsync

quota:                  # ★ 静默控制（已实现：Windows Job Object + 优先级）
  idle_threshold: 5m    # 空闲多久后进入 QUIET（滞回）
  quiet_cpu_cap: 5      # QUIET 态 CPU 硬上限 %（Linux 用 systemd CPUQuota）
  active_cpu_cap: 80    # ACTIVE 态 CPU 硬上限 %

policy:
  propagate_delete: false   # 历史遗留字段：必须 false（删除传播由 mirror 接管）
  rapid_upload: true

mirror:                 # ★ 镜像模式（v2，默认开启，见 §9.2）
  enabled: true         # true = 主树路径清单永远等于本地；旧版/删除/清单外 → 归档
  history_dir: ".history"  # 远端历史根目录（RA 内单级目录名；本地不允许同名目录，启动拦截）
  history_days: 90      # 保留期；0 = 永久；到期整日期桶服务器端清理
  on_rename: mv         # mv = 远端直接改名 | history = 归档+重传（降级）
  remote_reconcile: 24h # 远端清单对账周期（清单外 → 归档、缺失目录 → 补建）

exclude:                # 排除规则：单组件 glob 命中任一路径组件（深度不限）
  # - "*.tmp"
  # - "~$*"
  # - ".stfolder/"

service:
  log_file: "logs/bdsync.log"   # 相对配置文件目录 → <exe同目录>/logs/；空 = 只打 stdout
  log_max_mb: 10
  log_keep: 7
  db_path: "index.db"           # 相对配置文件目录 → 与二进制同目录

web:                    # ★ 本地看板（§13；凭据绝不进配置文件）
  enabled: false        # 或 daemon --web
  addr: "127.0.0.1:8787"  # 强制回环地址（Validate 拦截非回环）
```

### 14.1 配置驱动的部署契约（v0.6 追加）

**就地便携**：二进制与它的配置、索引库、日志在同一文件夹里，整个目录可以拷到任何地方
（甚至 U 盘）后用绝对路径启动，行为完全一致：

```
<任意目录>/bdsync/
  ├─ bdsync.exe
  ├─ bdsync.yaml      备份对在这里声明
  ├─ index.db         索引库（与二进制同目录）
  └─ logs/bdsync.log  日志与轮转（≤10MB × 7）
```

- **配置文件发现顺序**：`--config` > `<exe 同目录>/bdsync.yaml` > `./bdsync.yaml` >
  `./var/bdsync.yaml` > `<exe 同目录>/var/bdsync.yaml`（exe 目录优先：跟二进制一起走的那份才是本意）
- **相对路径锚定**：`db_path` / `log_file` / `pair.local_root` 的相对值一律以**配置文件所在目录**
  为基准解析，**与调用时的工作目录无关**（这是"从任何目录启动结果一致"的实现基础）
- **备份对必填**：`pair.local_root` 与 `pair.remote` 缺任一项 → **启动即拒绝**，并打印可直接抄走的示例。
  理由：备份目标必须是显式声明的事实，不猜、不兜底、不从命令行临时绑定（v1 的
  `scan --root` + `send --remote` 绑定流程因此取消，两个参数退化为"与配置一致时的显式重复"）
- **一致性强校验**：索引库已绑定的本地根/远端目录与配置不符 → 拒绝启动（防止一份库被悄悄改指到另一个目的地）

---

## 15. 实施路线

排除网络盘后只有两个检测后端，**Linux 版基本是 Windows 版的子集**（poll + 相同的 Sender/DAO/Schema）。路线因此更短。

### v2.1 记分卡（2026-10-14）

| 里程碑 | 状态 | 实测证据 |
|---|---|---|
| Phase 1 正确性基线（poll/DAO/串行 Sender/错误分类/dry-run） | ✅ | 43,746 文件全量扫描 1.13s 入库 / 稳态 595ms 零写库；小目录全流程真机 |
| Phase 2 Windows 静默化（USN 解析/电源状态机/daemon） | ✅ 代码+单测 | USN 记录解析 V2/V4+防御性截断单测；daemon 状态机真机（降权沙箱内卷句柄被系统拒 → 自动降级 poll 路径实测；usn 真实 journal 待普通权限环境验证） |
| Phase 3 性能（并发 AIMD/断点续传/对账周期） | ✅ | 4×8MB 并发交错 7.8s/32MB；秒传/直传/分片/下载校验 8/8 一致 |
| Web 看板（§13） | ✅ | SSE+状态+配置热更+暂停/停止，仅回环；四页 hash 路由 |
| 凭据净化（`SanitizeCookie`） | ✅ | 只留 BDUSS/STOKEN/BAIDUID + 逐字节剔除非法字符；老 credentials.dat 自愈，无需重粘 |
| **镜像 M1 归档原语+删除传播+覆盖前置归档** | ✅ | 真机：改/删 → `.history/日期/路径@r_mtime`，主树干净，内容校验一致 |
| **镜像 M2 改名远端 mv** | ✅ | 真机 e2e：Renamed=2 / Uploaded=0 / Archived=0，md5 保持，幂等可重跑 |
| **镜像 M3 prune+远端清单对账** | ✅ | 真机：未知文件/整棵子树归档、2019 桶清理、dry-run 预览一致；含一次误归档事故的完整恢复演练（秒传零带宽重建） |
| Phase 4 生产化（文件日志轮转 ✅ / 部署件 / 长跑验证 / 任务计划） | 🔶 | 日志轮转已实现+单测；systemd/任务计划脚本与 48h 长跑待做 |
| Phase 5 加固（verify --sample / Defender 引导 / --follow-links） | ⬜ | check 已具备全量校验能力，抽样与周期化待做 |

### Phase 1 — 正确性基线（poll only，跨平台）

1. Schema + DAO（增量 diff、原子领取、状态迁移、批量写）
2. `Backend` 接口 + `poll.go`（**portable，同时是 Windows 和 Linux 的实现**）
3. `SelfTest` 机制 + 启动时的本地磁盘校验
4. Sender：串行 + 完整错误分类 + 原子写回
5. `--dry-run` / `status` / `inspect` 三个命令

> **这一阶段交付的东西在 Windows 和 Linux 上都能跑，且功能完整。** 先用最简单的 poll 拿到正确性基线，跳过这步直接写 USN，出问题时无法判断是逻辑错还是检测错。

### Phase 2 — Windows 静默化

6. **USN 后端** ★ 一次性解决 CPU/IO/风扇问题
7. `dir_ref` 路径解析表 + 回绕/Journal ID 检测
8. 电源状态机 + Windows 优先级 / CPU cap / 内存修剪
9. Go 运行时调优（GC 阈值、buffer 池、预分配）
10. `settle` + `state` 状态机

### Phase 3 — 性能

11. Sender 并发 + AIMD + 优先级队列 + 断点续传
12. Bootstrap 远端一次性列举（并发 BFS + `Ifhassubdir` 剪枝）
13. 强制对账循环（§4.9）+ 自适应周期

### Phase 4 — 生产化

14. SSE 看板（1Hz，无连接不推）
15. 任务计划程序（Windows）/ systemd unit（Linux）
16. 长期运行清单逐项验证（§4.8）

### Phase 5 — 加固

17. Defender 排除项引导（Windows）/ 扫描器白名单（Linux）
18. `verify --sample` 抽样校验，建立信任基线

### 明确不做

- ❌ inotify 后端 —— 已评估（§4.10），本地 SSD 上收益为零而风险不为零。等实测发现 poll 在机械盘上不可接受时再加
- ❌ 网络盘支持 —— 见 §1.1
- ❌ 双向同步 —— 见 §1

---

## 16. 验收测试

上线前必须逐条跑过：

```
功能正确性
  [ ] 写入一个大文件 → 确认 settle 生效，上传的是完整文件（比对远端 md5）
  [ ] 传输中 kill 进程 → 重启后队列与 upload_id 完好，能续
  [ ] 手工在网盘删一个文件 → 单向策略下不触发任何动作
  [ ] 本地改名（含纯大小写改名）→ 远端正确更新
  [ ] 0 字节文件 → 不误判，能正常传输
  [ ] 中文 / emoji / 超长路径名 文件 → 路径映射正确
  [ ] 两次全量对账之间新增 500 个文件 → 下一轮对账能全部发现（无遗漏）

检测后端   ★ 新增
  [ ] SelfTest 通过；故意让后端失效（如把 root 指向 BitLocker 卷）
      → 必须降级到 poll 并告警，绝不静默继续
  [ ] 强制 poll 模式跑一周，与 usn 模式结果逐条比对 → 必须完全一致
  [ ] 删除 USN 游标记录后重启 → 自动全量扫描，无遗漏
  [ ] 修改 NTFS 卷（修复/格式化）→ Journal ID 变化被检测到，触发全量扫描
  [ ] reconcile_interval 设为 1 分钟跑 1 小时 → 无内存增长、无重复入队

静默性  ★ 本项目的核心验收项
  [ ] 空闲 10 分钟，任务管理器 CPU < 0.5%，内存稳定不增长
  [ ] usn 模式下空闲时磁盘完全不唤醒（听声判断）
  [ ] 一次全量扫描的 CPU 峰值 < 70%，且持续 < 10s
  [ ] 笔记本电池模式下表现一致
  [ ] 连续运行 48 小时，RSS 无增长趋势
  [ ] Linux: systemd status 显示 nice=10，idle 期 cpu 时间几乎不增长

范围约束   ★ 新增
  [ ] 传入网络路径（\\NAS\share 或 /mnt/nfs）→ 启动即拒绝，错误信息可操作
  [ ] 传入 U 盘路径 → 拒绝
  [ ] 传入本地 ext4/xfs 目录 → 正常接受

镜像 + 历史（v2 ★ 真机实测项标 [x]）
  [x] 修改文件 → 旧版进 .history/<日期>/，主树=新版（远端下载 md5 与本地一致）
  [x] 删除文件 → 原版归档进 .history，主树移除（31066）
  [x] 目录改名 → 远端整棵 mv（0 上传 0 归档），子文件逐个确认一致后撤回 IDENTICAL
  [x] 改名后又删除 → 归档源指向改名前位置（renamed_from）
  [x] 清单外远端文件 / 整棵未知子树 → 归档（dry-run 预览与实跑一致；.history 豁免）
  [x] 过期历史桶（>history_days）→ 整桶清理；未过期与非日期命名目录保留
  [x] 误归档恢复演练 → 全部重入队 + 秒传零带宽重建主树（10 秒传 + 1 直传）
  [x] 本地存在同名 .history 目录 → 启动/扫描拦截
  [ ] 真实 90 天后过期桶自动清理（长周期观察）
  [ ] usn 真机改名端到端（需普通权限环境；poll 降级路径已验证）

Web 看板（v2.1）
  [x] 仅监听 127.0.0.1（配置校验回环 + 请求来源校验 + Origin 同源）
  [x] 暂停 → 不扫描不上传且进 QUIET 修剪；继续 → 下周期立即检测；停止 → 优雅退出
  [x] 日志 SSE 实时流 + 500 行断点回填；配置保存热更新子集、结构性字段提示需重启
  [x] 四页 hash 路由（概览/配置/日志/关于），刷新与前进后退对得上；版本号只在关于页
  [x] 关于页可读（纯静态内容，不依赖任何 API 请求即可展示核心逻辑）

长期运行
  [ ] 休眠 → 唤醒 → 自动走全量扫描且不漏文件
  [ ] 断网 1 小时 → 恢复后自动续传
  [ ] 系统时间向前/向后跳 1 小时 → 不误判
  [ ] 数据库 WAL 增长可控（定期 checkpoint）
  [ ] 日志轮转正常，磁盘占用有上限

安全
  [ ] 同步目录已加入 Defender 排除项
  [x] Web 看板仅监听 127.0.0.1
  [x] 凭据（BDUSS/STOKEN）只从环境变量读取，绝不进配置文件/数据库/日志
```

---

## 17. 运维答疑（实测复核过的常见疑问）

> 本节记录几个反复被问到、且**容易被直觉答错**的问题。每条结论都标注了代码依据与实测数据，
> 避免"文档这么写"和"代码真这么跑"长期脱节。数据取自一台 Windows 11 + SVIP 账号的实测环境
> （约 43,700 文件 / 89 GB 目录树，本地 NTFS → 百度网盘），2026-10-14。
> 文中出现的本地/远端路径均为**泛化占位符**，非真实部署路径。

### 17.1 传输是单线程的吗？—— 不是，是 12 路并发

上传器是**网络密集**的，Detector 与 Sender 之间只有 SQLite、没有任何内存队列
（§3 / §7 并发模型）：

```go
// internal/sender/sender.go:318-380
claimSize := s.lim.limit() * 2          // 12*2 = 24
batch, _ := s.st.ClaimBatch(ctx, claimSize)
for ... {
    wg.Add(1)
    go func() {                          // 每条记录一个 goroutine
        s.lim.wait(ctx)                  // ← 并发名额在这里限
        s.uploadOne(ctx, e, stats)
    }()
}
wg.Wait()
```

并发额度由 AIMD 限流器管（§12），起步 `min(12, concurrency)`。配置 `concurrency: 12` 时
**起步就是满值**，因为 `cur(12) < max(12)` 不成立，永远不会再涨 —— 日志里"并发提升"0 次是正常的。

**实测证据**（单次 drain，每秒完成数分布）：

```
 6 条/秒 ← 22 个秒点     11 条/秒 ← 38 个秒点
 9 条/秒 ← 30 个秒点     12 条/秒 ← 17 个秒点   ← 12 正是限流器上限
10 条/秒 ← 32 个秒点     峰值 18 条/秒
平均 6.47 条/秒
```

峰值 18、单秒簇集中在 11–13 —— 单线程不可能是这个形状。

**"感觉像单线程"的真实原因**：那次 drain 的汇总是

```
2328 文件 / 430.01s = 5.41 文件/秒   传输 8.3 MB (0.02 MB/s)
```

**2312 条是秒传（传 0 字节，只做 md5 哈希 + 一次 API 查询），真正上传的只有 16 条。**
瓶颈是**每个文件的 API 往返延迟**，不是带宽。盯着网盘客户端看带宽必然是平的。

**已知效率损失**：`claimSize = limit*2` 配上批尾的 `wg.Wait()`，每轮 24 条是一个栅栏 ——
最慢的那个文件没跑完，后面的名额就空转。改成滑动窗口（channel 喂任务、不做批栅栏）
能让名额始终满载。**未做**，留作后续优化。

### 17.2 第一次全量运行做了什么

以真机首跑为例，序列如下：

1. **向导写配置** —— 置 `service.initialized: true`，之后才进服务态
2. **绑定远端根** —— `SetRemoteRoot`（`store.go:452`）**首次绑定后不可改**
3. **启动即全量扫描**（`daemon.go:414`，无条件执行）
   - `NextRunID` 取新轮次号
   - `ScanAll` 走完整棵目录树，与索引 diff `(size, mtime_ns)`
   - 索引为空 → 每条都走 `queueState()` 判为 `LOCAL_NEW`（`poll.go:137`），一次性灌进队列
   - **同时建立 `dir_ref` 表**（目录的 NTFS FileReferenceNumber → 相对路径）。
     这一步**不能省** —— 缺了它，后续 USN 会把新文件的父引用号解析不到、当成"根之外"忽略，
     **静默漏文件且不报错**（`usn_windows.go:302` 的注释就是在说这件事）
4. **启动即远端清单对账**（`daemon.go:418`）—— 列举远端全树与本地清单比对，
   **清单外的远端项一律归档、不删除**（§9.2 决策 Q2）
5. **进主循环** —— USN 首轮 `storedJID == 0` → 只记游标、**不产生任何事件**
   （现状已由第 3 步覆盖）；drain 双触发：到 `flush_interval` 或积压 ≥ 高水位

实测：启动 20:15:47 → 20:22:55 开始首个 drain（发现 39,745 文件 / 81.1 GB）。
其中约 4 分钟花在远端对账列举上，不是本地扫描。

**⚠ 已知问题**：远端对账的并发是**写死的 8**（`mirror.go:358 const workers = 8`），
**不走 AIMD 限流器**（限流器只管 `uploadOne`）。首跑遇到远端已有大量内容需要归档时，
它会不管上传侧的限流状态直接 8 路打百度 —— 实测 79 次 `31034 pcs refuse service`。
不会丢数据（归档失败绝不改行状态，§9.2 毒丸防护），但每个对账周期会重复失败一轮。

### 17.3 休眠中断后，下次启动会怎样

**三重兜底**，其中第一条与 USN 完全无关：

1. **启动无条件跑全量扫描**（`daemon.go:414`）—— 没有前置条件，休眠期的所有变更由
   WalkDir 兜住，与游标状态、journal 是否回绕都无关。**这一条就关掉了"漏文件"风险。**
2. **`IN_PROGRESS` 残留自动复位**（`store.go:507`）——
   ```sql
   UPDATE entry SET state = CASE WHEN r_fsid IS NULL THEN 2 ELSE 4 END
   WHERE pair_id=? AND state=7
   ```
   休眠时被杀的条目停在 `state=7`，下次 `ClaimBatch` 开头打回 `2/4` 重新入队。
   半传完的可能重传一遍，但不会永久卡死（§9.3「宁可多传，不漏传」）。
3. **断点续传接得上** —— `upload_id` + `done_blocks` 持久化且随条目查回。
   注意这条复位**只改 `state`，不清断点**；只有扫描发现**内容变了**才清
   （`store.go:300`）—— 单纯中断不该作废断点，内容变化才该。

另外 USN 游标回绕会**再触发一次**全量扫描（`usn_windows.go:313`），日志：
`[!] usn 游标失效/journal 重建（正常路径）→ 全量扫描`。
这次与第 1 步重复，属冗余但无害（poll 全量约 0.5s）。

### 17.4 「每次都全量对账，永远跑不完」—— 在收敛，别被重启误导

队列深度实测（同一小时内）：

| 时间 | 队列 | 体积 | 本轮完成 |
|---|---|---|---|
| 20:22:55 | 39,745 | 81.1 GB | — |
| 21:19:38 | 39,076 | 80.9 GB | 36 |
| 21:38:27 | 38,622 | 80.7 GB | 2,328 |
| 21:49:53 | 36,297 | 79.8 GB | 6,840 |
| 22:14:58 | **29,457** | **75.9 GB** | — |

1h52m 掉了 10,288 个文件 / 5.2 GB。最后一轮可对账：36,297 − 6,840 = **29,457**，
与日志完全一致，说明那 25 分钟内**零新增**。drain 速率 6,840/1,475s = **4.6 文件/秒**。

**"一直全量对账"是重启次数的函数，不是同步慢**。90 分钟内 daemon 启动了 12 次，
每次都触发一轮全量扫描 + 远端对账，并打断正在跑的 drain。12 次里只有 1 次留下了
`daemon 退出（停止请求/信号）`，其余 10 次无退出日志 —— 猝死。

首跑时队列有几万条，反复重启确实会让人以为"永远跑不完"。让进程别动、或交给
任务计划程序托管（§11），中途不重启即可。

### 17.5 重启后队列是重置还是恢复？—— 恢复

队列在 SQLite（`index.db`，exe 同目录）里，不在内存。每个文件传完**立刻**写
`state=IDENTICAL`（`store.go:300`），不是攒批再写。

真机 `bdsync status` 当时的状态分布：

```
ByState: { "1": 15778,  "2": 34235,  "7": 1 }
Files: 43772   pending: 27993
```

- **`state=1`（IDENTICAL）15,778 条** —— 跨 12 次重启累计下来的已完成记录。
  队列若每次重置，这个数永远是 0。
- **`state=7` 恰好剩 1 条** —— 上次被杀时没来得及回写的那一个。**它静静躺在磁盘上**，
  证明进程状态是被持久化的。

**唯一被"重置"的是 `state=7`**，且是修正而非丢失（见 17.3 第 2 条）。

全代码库无启动期的破坏性操作：`DELETE FROM entry` 只有两处，都在**改名**路径
（`store.go:695`/`721`），与启动无关；`TRUNCATE` 只出现在 `PRAGMA wal_checkpoint`。

**关键机制**：启动的全量扫描是 **diff 不是重排** —— 只有 `(size, mtime_ns)` 变了才
重新入队，已 `IDENTICAL` 的文件识别为「未变」直接跳过。所以"每次全量"扫的是目录树，
不是重排队。

### 17.6 日志长时间没动静，是死了吗？—— 没有，但要看对地方

P2 要求「无变更时绝对空闲」，所以**真正常驻的实例日志会长时间没有新行**。实测静默段：

```
10-14 22:47 → 10-15 00:31    103.8 分钟
10-15 01:43 → 10-15 03:09     86.2 分钟
10-15 05:30 → 10-15 06:23     52.8 分钟
…（共 12 段 ≥10 分钟）
```

2026-10-15 07:04:25 进入 QUIET 后静默 73.9 分钟，其间进程健康（CPU 累计 1272s / 10.5h
≈ 3.4%，RSS 43.5 MB，队列已排空）。**只看日志文件无法区分「安静地活着」和「卡死/被杀」。**

代码里原本唯一的 `heartbeat`（`web.go`，15s）是 **SSE 注释帧**，只在浏览器连着看板时发，
且从不写日志文件。

**v2.3 起加了静默期心跳**：只在 QUIET / PAUSED 时每 30 分钟打一行，忙时一行不多打。

```
[心跳] QUIET：静默 2h13m · 队列 0 项（0 B）· RSS 43.5 MB · 后端 usn · 已运行 10h37m
[心跳] PAUSED（已暂停）：静默 45m · 队列 12 项（3.2 MB）· RSS 44.1 MB · 后端 usn · 已运行 11h2m
```

其他判活方式（都不依赖日志）：

| 方式 | 说明 |
|---|---|
| 看板概览页 | 「运行时长」+ SSE 连接小圆点（变红 = 连不上） |
| `GET /api/status` | `uptime_sec` 单调递增即活着 |
| `Get-Process bdsync` | 最直接，能看 CPU/RSS |
| `bdsync status --db index.db --json` | ⚠️ **只读数据库，不含存活信息** —— 进程死了照样返回数据，**不能用来判活** |

### 17.7 待定：远端对账会补建空目录

与 17.2 记录的「归档后清理空目录」是**两条不同的路**，症状一样：

| 路径 | 建空目录？ | 原因 |
|---|---|---|
| 上传 `ensureDirs` | ❌ | `ClaimBatch` 只取 `is_dir=0`；只建"批内文件的父目录" |
| 远端对账 `ReconcileRemote` | ✅ | 补建段遍历 `LocalManifest`，**不区分有无文件后代** |

`LocalManifest`（`store.go:988`）返回所有 `state != 9` 的路径。所以每次启动对账 + 每
`mirror.remote_reconcile` 周期，会把本地**本来就没有文件**的空目录建到网盘上。

实测规模：**965 个空目录**（按顶层目录分布：912 / 29 / 16 / 8）。

v2.3 的空目录清理**不覆盖这一类** —— 它们没有文件，不在 `emptied` 集合里。

**取舍待定**：
- *改*：对账时跳过无文件后代的目录 → 与空目录清理合起来构成完整的「不产生、产生的清掉」，
  代价是放弃「完全映射」，网盘上不留空目录骨架
- *不改*：保留空目录镜像（部分场景需要保留目录结构）

**当前维持原状**（仍会补建），因为这是个行为变更而非缺陷修复，且 git 式的"不跟踪空目录"
是否适用于备份场景尚无定论。

---

## 附录 A · 关键 Win32 常量备忘

```c
// FSCTL
FSCTL_USN_READ_JOURNAL   0x000900BB   // 读 journal 元数据
FSCTL_READ_USN_JOURNAL   0x000900A4   // 读变更记录

// USN Reason 位
USN_REASON_DATA_OVERWRITE   0x00000002
USN_REASON_DATA_EXTEND      0x00000001
USN_REASON_FILE_CREATE      0x00000100
USN_REASON_FILE_DELETE      0x00000200
USN_REASON_RENAME_OLD_NAME  0x00001000
USN_REASON_RENAME_NEW_NAME  0x00002000

// 优先级
IDLE_PRIORITY_CLASS        0x00000040
BELOW_NORMAL_PRIORITY_CLASS 0x00004000
NORMAL_PRIORITY_CLASS      0x00000020
ABOVE_NORMAL_PRIORITY_CLASS 0x00008000

// Job Object
JOB_OBJECT_CPU_RATE_CONTROL_ENABLE    0x00000001
JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP  0x00000004
// CpuRate 单位 = 1/100 百分比 → 10000 = 100%，1500 = 15%

// ReadDirectoryChangesW
FILE_ACTION_ADDED           1
FILE_ACTION_REMOVED         2
FILE_ACTION_MODIFIED        3
FILE_ACTION_RENAMED_OLD_NAME 4
FILE_ACTION_RENAMED_NEW_NAME 5
ERROR_NOTIFY_ENUM_DIR       1450   // 缓冲区溢出 → 必须全量重扫

// 打开卷 / 目录
CreateFile("\\\\?\\C:", GENERIC_READ, FILE_SHARE_*…, OPEN_EXISTING, 0, NULL)

// 磁盘类型校验（排除网络盘 / 可移动盘）
DRIVE_FIXED     3   // ✅ 唯一接受的
DRIVE_REMOVABLE 2   // ❌ U盘
DRIVE_REMOTE    4   // ❌ 网络盘

// GetDriveTypeW 返回值用于启动时校验，见 §1.1
```

> FSCTL 与结构体布局随 Windows 版本演进（USN_RECORD_V2 需要 Win8.1+）。
> 优先用 `github.com/ricochet2200/go-diskwatcher` 或成熟实现，或对 `USN_RECORD_V4` 做长度自适应的防御性解析。

---

## 附录 B · 决策记录

| 决策 | 理由 | 替代方案及为何否决 |
|---|---|---|
| **不支持网络盘** ★ | 网络盘上文件变动无法可靠检测，会导致备份静默遗漏 | 支持它：必须引入有损后端 + 强制对账兜底，复杂度翻倍。当前场景（本地 NTFS）用不上 |
| **不实现 `ReadDirectoryChangesW`** | 天生有损：缓冲区溢出、改名单槽配对会丢事件 | 实现它：唯一优势场景是网络盘，已排除。在 NTFS 上被 USN 全面压制（§4.3） |
| **不实现 inotify** | 本地 SSD 上 poll 1–3s/5min ≈ 1% CPU，与 inotify 阻塞等待无差别；但 poll 零正确性风险、代码量少 2/3 | 实现它：引入 watch 失效/配额/溢出等一整类新故障模式，换不到可观测的收益 |
| USN 为 Windows 主后端 | 零 I/O、零 CPU，与目录规模无关，且丢失可检测 | 纯 WalkDir：本地 SSD 上 1–2s 扫描，产生真实磁盘活动 |
| **Linux 默认 poll** | Linux 无 USN 等价物；poll 在本地 SSD 上代价可接受且天然完整 | inotify：主要价值在网络挂载（已排除）与超高频写入（不适用） |
| `SelfTest()` 为接口必实现 | 防止"调用成功但从不通知"的静默失效 | 依赖后端自身校验：环境变化（加密卷、换盘、企业策略）时无从发现 |
| 判定逻辑只读索引，不读事件流 | 让任何后端都只是性能优化，不可能引入正确性 bug | 从事件流直接推断状态：后端实现细节渗透进核心逻辑 |
| poll 实现永不删除 | 既是降级路径，也是 usn 的正确性对照组 | 只留 usn：无降级路径，且出 bug 时无法定位是检测层还是比对层 |
| ~~不传播删除（v1）~~ → 已被"镜像 + .history 归档"取代（见下） | v1 视 rsync 式删除传播为备份大忌；v2 用"先归档再删"两全：主树镜像 + 历史可找回 | rsync 默认裸删：不可找回；v1 的"完全不删"：主树漂移。两者取中 |
| SQLite 而非内存索引 | 崩溃安全 + 低内存（usn 模式无需常驻全表） | 纯内存：进程崩溃丢失队列，违背 P3 |
| 单表 + 部分索引 | `WHERE state NOT IN (1,9)` 让 4 万条 IDENTICAL 不进索引 | 独立 pending 表：多一次 join，收益不明显 |
| 秒传计算用 256KB 流式 buffer | 内存 4MB 而非 64MB | 整块 4MB 驻留：16 并发下多占 60MB |
| 先 poll 后 usn | 先建立正确性基线 | 直接上 USN：出 bug 时无法定位层次 |
| Web 看板 1Hz / 无连接不推 | 60fps 是 CPU 大头 | 常驻推送：为了没人看的页面烧 CPU |
| CPU cap 分状态设置 | 空闲 5% 无副作用，活跃需放开 | 全局 15%：会拖慢哈希进而拖慢上传 |
| 权限/xattr/ACL 不备份 | 目标网盘只存字节，这是存储介质的固有限制 | 尝试备份：无处可存。需显式声明范围而非假装支持（§11.1） |
| **镜像 + .history 语义取代"绝不传播删除"（v2，Q3 默认开启）** | 用户主诉求是 folderRA 永远映射 folderLA；数据安全改由".history 归档 + 90 天保留"承担，比"不删"更强（旧版可找回且主树永远干净） | 保持 v1 纯备份：远端主树与本地漂移、改名/删除不传播，用户需另找镜像工具；现保留 `mirror.enabled: false` 回退 |
| **归档 = 服务器端 Move（非 Copy+删）** | 一次调用同时完成历史写入与主树删除，零带宽、原子、无中间态 | Copy 后再删：双倍流量、两步之间有丢失窗口、配额瞬时双份 |
| **改名走远端 mv（Q1）** | 内容不变：零历史污染、零带宽、一次调用；降级链（poll 无改名事件/目标冲突）自动退回归档+重传 | 一律归档+重传：实现单一但每次改名产生一份内容不变的历史副本 + 秒传调用 |
| **清单外远端项 → 归档而非删除（Q2）** | 主树路径清单严格 ≡ 本地清单的同时零丢失风险（其他客户端写入的东西 90 天内可找回） | 不碰：主树不收敛；严格删除：误删不可逆，风险不可接受 |
| **归档命名确定性（路径@r_mtime）+ 先查后动** | 幂等三步让崩溃/重试永不产生重复归档；同文件同日多次归档自然不撞名 | 随机后缀/时间戳后缀：重试产生孤儿副本，需额外清理逻辑 |
| **误归档恢复 = 重入队 + 秒传重建** | 内容仍在服务端（归档副本），秒传按内容 md5 命中，零带宽恢复主树；已真机演练固化 | 逐个 Move 搬回：N 次 API 调用且需维护反向映射 |
| Web 看板内嵌 daemon 进程 | 控制面（暂停/停止）直连状态机，零 IPC；日志事件源同进程 | 独立看板进程：需要跨进程控制通道，复杂度翻倍 |
| 凭据只认环境变量（配置文件禁存 BDUSS/STOKEN） | 配置文件会被拷走/同步/提交；环境变量是本机进程级 | 配置存凭据：泄密面扩大到所有接触配置文件的路径 |

---

## 附录 C · 代码审查修复记录（2026-10-16）

一次全量代码审查（五模块逐文件精读 + 高危结论人工复核）后的修复记录。分三档：
**P0** = 数据丢失/不一致/崩溃/安全漏洞；**P1** = 可靠性、并发、资源隐患；**P2** = 可维护性与加固。

### C.1 本轮修复清单

#### 看板安全（`web/web.go`）

| 项 | 档 | 修复 |
|---|---|---|
| `withLocal` 缺 Host 校验 | P0 | 原防线（RemoteAddr 回环 + 以 `r.Host` 为基准的 Origin 前缀匹配）挡不住 DNS rebinding：恶意页面把域名解析到 127.0.0.1 后，请求经回环进来、Origin 与 Host 自洽，两道校验全过。现在把 `r.Host` 钉死在监听地址上（`allowedHost`），并对写请求强制要求同源 Origin。 |
| 写请求无 Origin 时放行 | P1 | 改成：非 GET/HEAD 必须带同源 Origin；脚本类客户端用自定义头 `X-Bdsync-Local` 显式声明（跨源携带该头会触发 CORS 预检，而本服务不返回任何 CORS 允许头 → 恶意网页伪造不了，不削弱防护）。 |
| 监听地址未强制回环 | P1 | 配置若写成 `":8787"`，`net.Listen` 会静默绑全网卡。现在 listen 后校验实际地址，非回环直接拒绝启动。 |
| `isLoopback("")` 返回 true | P2 | 去掉空主机名放行分支。 |
| `/api/setup/probe` 接受任意路径 | P2 | 限定 GET + 必须为绝对路径。 |

#### 数据一致性（`internal/index` + `internal/sender`）

| 项 | 档 | 修复 |
|---|---|---|
| 上传回写无版本守卫 | P0 | `FinishUpload`/`FailUpload` 的 WHERE 原本只有 `pair_id+rel_path`。大文件上传可能持续数分钟，期间本地被再次修改（Detector 已写回"待传"并更新 `l_mtime_ns/l_size`），此时无条件回写 IDENTICAL 会让那个新版本**永不重传**。现在 WHERE 带上领取时的 `state=IN_PROGRESS` + `l_mtime_ns` + `l_size` 快照，`RowsAffected()==0` 即保留新状态。 |
| 归档目标名撞名 | P1 | 目标名是 `<rel>@<r_mtime>`（秒级），幂等检查"目标存在 = 已归档"。`r_mtime` 缺失（0）时所有版本会撞成 `@0`，误判已归档后清账 → 旧版丢失。现在 `r_mtime<=0` 时用当前时间兜底：宁可留下重复归档（源已移走时第二次无害返回），也不静默丢版本。 |
| attempts 超限静默丢弃 | P1 | `pendingSelect` 用 `attempts < 20` 排除，失败行会永久停在 ERROR：既不上传也不报错。现在提取常量 `index.MaxAttempts`，`PendingStats` 新增 `Blocked` 口径，daemon 心跳/排空日志、`status` 命令、看板 JSON 全部显式报出。 |
| 归档失败计入上传失败 | P1 | 归档失败会 `attempts+1`，20 次后被静默丢弃。归档是"覆盖前的保护动作"，其失败不该消耗上传配额（改动为 UNSTABLE 语义，不递增 attempts）。 |
| USN 同轮"删后重建"被丢弃 | P1 | 收尾时把 `deleted` 中的路径从 `changed` 整体剔除，导致重建出的新文件既不入删除语义也不被重传。改为两条集合各自上报，分流交给 `ApplyChanges` 的 `os.Lstat` 判定。 |
| poll 子树不可读 → 假删除 | P1 | 子目录 Access Denied 后 `fs.SkipDir`，整棵树不进 `seen`，删除判定把它们全部标为 `REMOVED_LOCAL`。现在标记 `walkIncomplete`，该轮只跳过删除判定（本轮检测到的变更仍照常提交，下轮自愈）。 |
| 对账无安全阀 | P0 | 对账以本地清单为唯一真值来源，索引异常（库损坏/pair 未初始化/检测未跑完）时清单变短即导致**整棵主树被搬进 `.history`**（`recovery_live_test.go` 记录过真实事故 M3）。现在归档前统计 `victims/cand`，超过 `max(20, 50%)` 直接中止并报错，不静默降级。 |
| 空目录删除的 TOCTOU | P1 | `IsEmpty` 与 `Remove` 之间网盘侧新放入的文件会被递归删掉。修复方式为删除前复核（缩小窗口）+ 已知残余风险记录（见 C.3），不改变外部行为。 |

#### 检测层（`internal/detector`）

| 项 | 档 | 修复 |
|---|---|---|
| USN 解析先读后校验 | P0 | `le.Uint64(rec[16:])` 在 `len(rec)<60` 检查**之前**执行，`recLen∈[16,23]` 的畸形记录直接 panic（越界），把常驻进程带走。改为长度前置校验 + 显式切片上界。 |
| `GetKV` 读错误被吞 | P1 | 读失败被当成 0，命中"首次运行"分支后把游标重置到 `NextUsn` → 该区间全部变更被静默跳过。现在区分"键不存在"（`(nil,nil)`）与读失败（上抛）。 |
| 空闲时仍写游标 | P1 | 无新记录也每轮 `saveCursor`（2 次 KV 写），持续触碰 WAL，与"空闲零唤醒"冲突。改为仅在游标推进时落库。 |
| **元数据查询用错 FSCTL** | P0 | `readMeta` 调用的是 `0x000900BB`，而那是 `FSCTL_READ_USN_JOURNAL`（要求 40 字节输入结构）；用空输入调用必然返回 `ERROR_INVALID_USER_BUFFER`，于是**元数据查询在启动时就失败、整个进程永久降级为 poll**。真正的 `FSCTL_QUERY_USN_JOURNAL = 0x000900F4`。这是本轮最严重的发现：USN 后端自始未曾生效，"空闲零唤醒"的核心前提实际不成立。 |
| **读记录 IOCTL 值错误 + 输入缓冲未对齐** | P0 | `readBatch` 的 `0x000900A4` 并非任何 READ_USN_JOURNAL；且 `FSCTL_READ_USN_JOURNAL` 是 `METHOD_NEITHER`（驱动直接访问用户内存），栈上的 `[40]byte` 只按 1 字节对齐，可能被驱动以"用户缓冲无效"拒掉。改为正确值 `0x000900BB` + 堆分配（分配器保证 8 字节对齐）。 |
| **JournalID 解析偏移错误** | P0 | 原实现把"`@0` 是 V2 才有的 JournalID"当作前提，在低字节数分支里把 `@0` 读作 `FirstUsn`。实际 `USN_JOURNAL_DATA_V0` 自 `@0` 起就是 JournalID（前 56 字节 V0/V1/V2 一致）。若走到该分支，JournalID 恒为 0 → 每轮都命中"首次运行"分支 → 永不产出变更。现在统一按 56 字节布局解析，输出缓冲留足 V2 的 88 字节。 |
| **移出监控根的重命名未按删除上报** | P0 | `resolve` 新路径失败（目标在监控根之外，例如把文件拖进回收站、跨目录/跨盘移走）时直接 `continue` 忽略 ---- 源路径于是既不进 changed 也不进 deleted，**索引永远保留该行、远端残留旧副本且毫无症状**。实测：删除文件后索引状态不变（`state` 仍为 `LOCAL_NEW`）。现在解析不到目标即把源路径按删除上报。 |

#### 网络层（`internal/baidu`）

| 项 | 档 | 修复 |
|---|---|---|
| 分片上传 client 无 Timeout | P1 | 只配了 `ResponseHeaderTimeout`，socket 写阻塞无期限 → 一次卡死永久占用 worker。加 `Timeout: 15m`。 |
| 上传回调 reader 不可重放 | P1 | SDK 重试时 `io.Copy` 返回 `(0,nil)` **不报错** → 空分片当成功提交，文件被截断。现在比对字节数，不足即显式失败。 |
| 降级下载可能死循环 | P1 | 循环唯一出口是 `n==0`；服务端忽略 Range 返回 200 时 `size<=0` 永不退出、已知大小时每窗口重下整包。现在带 Range 时强制 206 + 轮次上限 + 超长检测。 |
| 整包下载不校验长度 | P1 | 快路径只判 err（降级路径却判了）→ CDN 静默截断返回错误 md5。改为同时校验 `n==size`。 |
| `AsPCSError` 直接断言 | P2 | 改 `errors.As`，避免错误被 `%w` 包装后 `RemoteMissing`/31061 幂等判断静默失效。 |

#### 启动与运维（`cmd/bdsync` + `internal/logx`）

| 项 | 档 | 修复 |
|---|---|---|
| 主循环无 panic 兜底 | P1 | HTTP 层有 recover，主循环没有 → 检测/对账/排空的任一 panic 直接带走进程。新增 `safeRun` 包裹四处外部调用（周期对账、变更检测、远端对账、排空上传）。 |
| 未启看板时无单实例保护 | P1 | 单实例原本只靠看板端口占用。新增锁文件（Windows：share mode=0 独占打开；其他平台：`flock`），**锁随进程退出由内核释放**，因此崩溃残留的锁文件不会阻止下次启动 ---- 不依赖 PID 判定，无需清理逻辑。 |
| 空闲态每秒查库 | P1 | 每 tick（1s）无条件 `PendingStats`。改为活跃期即时、空闲期按 `flush_interval`（上限 30s）刷新；到点时必然已刷新，drain 触发时机不受影响。 |
| 索引查询未走索引 | P1 | `PendingStats` 的 `SUM(CASE...)` 不可 sargable（每次 drain 触发全表扫描）；`pendingSelect` 的 `ORDER BY l_settled_ns` 与 `idx_pending` 第三列 `l_mtime_ns` 不匹配（每次领取都要物化排序）；`state=9` 被部分索引排除却正是归档积压口径。新增 `idx_pending_order` / `idx_history` / `idx_blocked` 三条部分索引，并把统计拆成三条可命中索引的查询。 |
| PRAGMA 未写入 DSN | P1 | 只在 open 后执行一次，连接回收重建即丢失（`busy_timeout` 归零 → 立刻 BUSY）。改写入 DSN（`_pragma=...`）+ `SetMaxIdleConns(1)`。 |
| 统计字段数据竞争 | P1 | `Stats.FatalError`/`Stats.Claimed` 由 worker 加锁写、主 goroutine 裸读；`commitCount/commitDur` 裸自增。分别改为加锁 getter 与 `atomic.Int64`。 |
| `liveHeap()` 强制 GC | P1 | 看板每次轮询状态都触发一次全量 GC + STW。改为只读 `MemStats`（要"GC 后"口径请显式用 `sampleMem`）。 |
| 日志轮转失败即永久失效 | P1 | 轮转先 `Close()` 但未置空句柄，`os.Rename` 失败后 `open()` 不再执行，后续写入全打在已关闭 fd 上且无告警。现在每条失败路径都保证句柄置空 + 向 stderr 告警。 |
| 错误被静默吞掉 | P1 | `FailUpload/DeferEntry/MarkMissing/SetLocalMD5/SaveProgress` 的返回值多处丢弃。现在关键路径都带 `rel_path` 记日志。 |
| 中途重试不复用已传分片 | P1 | `doneSet` 只来自领取快照，`persistProgress` 只写库不回写内存 → 分片失败重试从 seq 0 重传。现在同步回写内存。 |
| `ctx` 取消无法中断在途上传 | P1 | 分片循环内不检查 ctx，关停/致命错误后单个大文件仍会跑完整段。循环内加检查，取消时保留已传分片供续传。 |

#### 配置与 CI

| 项 | 档 | 修复 |
|---|---|---|
| YAML 未严格模式 | P2 | 拼错键静默忽略（用户以为改了配置，实际按默认跑）。`Load` 启用 `KnownFields(true)`。 |
| `history_days` 优先级判据错误 | P2 | 原判据是"值 ≠ 默认 90"，导致显式写 `0`（永久保留）被 `quota.history_days` 静默覆盖。改为按"键是否出现"判定优先级，并同步 `bdsync.example.yaml` 的说明。 |
| `ParseByteSize` 接受负数 | P2 | 负值会让 drain 触发条件恒真/恒假。加非负校验 + `Validate` 补 `FlushBytes > 0`。 |
| 示例配置缺 `web:` 段 | P2 | 补上，并说明"只监听回环"的安全约束。 |
| `netdiskname` 校验缺口 | P2 | 补控制字符（`<0x20`、`0x7f`）与单组件长度（255 rune）拦截。**未做** Unicode NFC 归一化：会改变已有文件的远端路径、触发大规模重命名，风险高于收益。 |
| CI 脚本注入 | P2 | `release.yml` 把 `${{ steps.ver.outputs.tag }}` 直接插值进 bash（tag 来自 `GITHUB_REF_NAME`，git ref 名允许 `$ ( ) ; \` \|`，校验正则又允许 `[-+].*` 后缀）→ 能推 tag 者可在 runner 上执行任意命令。改为经 `env:` 传值，`grep -qa` 改 `-qaF`。 |
| CI 无质量门禁 | P2 | 新增 `go test -race ./...` 与 `go vet ./...`（仓库 8 个测试包此前从未在 CI 跑过）。`*_live_test.go` 由环境变量门控，无凭据时自行 Skip。 |
| 硬编码 app_id | P2 | 向导路径的 `266719` 改为 `baidu.DefaultAppID`，避免两处漂移。 |
| UPX 无来源校验 | P2 | **未修**：不凭记忆填 sha256。已在 workflow 内留明确注释说明补齐方式（见 C.3）。 |

### C.2 对使用者的行为变更

1. **看板写接口需要同源 Origin**：浏览器看板不受影响。若有脚本/curl 调用 `/api/control`、`/api/config`、`/api/credentials`，需加请求头 `X-Bdsync-Local: 1`（或用 `-H "Origin: http://127.0.0.1:8787"`）。
2. **单实例**：同一 `db_path` 只允许一个进程。第二个进程会明确报错退出，而不是静默双写索引。锁文件为 `<db_path>.lock`。
3. **配置拼错键会报错**：YAML 中出现未知键将拒绝启动并指出键名（此前静默忽略）。
4. **阻塞项会被报出来**：连续失败达到 `index.MaxAttempts`（20）的文件不再自动重试，但会出现在 `status` 输出、心跳日志与看板 JSON 的 `pending.blocked` 字段中。
5. **对账护栏**：清单外条目超过半数且多于 20 项时对账中止并报错，需人工确认索引状态（这是保护，不是故障）。

### C.3 已知未修项（留有明确记录）

| 项 | 为何不修 |
|---|---|
| `synchronous=NORMAL` 与"断电不丢变更"的措辞 | 改 `FULL` 会给每次提交加 fsync，直接冲击吞吐与磁盘唤醒目标。**准确的承诺是**："进程崩溃（panic/被杀）不丢已提交的变更；操作系统断电/掉电可能丢失最后一次 checkpoint 之前的批次"。要断电级保证可自行改 DSN 里的 `_pragma=synchronous(FULL)`。 |
| 空目录删除的 TOCTOU 残余 | 检查为空与执行删除无法原子化。已加删前复核，残余风险在"复核后、Remove 前"的毫秒级窗口。彻底消除需改为"空目录也先归档"，会改变 `.history` 的内容语义，需单独立项。 |
| 变更判定不比对 FRN | 修复需索引新增 FRN 列 + 迁移 + poll/usn 两侧比对改造。"删除后用相同 size 且保留 mtime 重建同名文件"的场景较少见；本轮先在检测侧记录该已知限制。 |
| junction/reparse point 的越界扫描风险 | 取决于目标 Go 版本对 `fs.DirEntry.Type()` 是否对 junction 置 `ModeSymlink`，需在真实环境实测后才能确定修法。 |
| `appCfg` / `appCfgPath` 的包级并发读写 | 需把 70 处 `appCfg.X` 机械替换为访问器调用，改动面大于本轮收益（实际影响是看板保存配置与状态读取的极端时序下可能读到半更新配置，属低频操作）。 |
| `CheckList` 的 N+1 查询、`NextRunID` 非原子、`ClaimRenamed` 无崩溃标记 | 均为低影响项，改动收益小于回归风险，留待后续需要时处理。 |
| CI 的 UPX 与 action 版本固定 | 需要核对官方 checksum/SHA 后才能填值，不凭记忆填写。 |

### C.4 验证方式与结果

```bash
go build ./...        # 通过
go vet ./...          # 通过
go test ./...         # 8 个测试包全部通过（含新增 FinishUpload 快照守卫回归测试）
go test -race ./...   # CI 执行（ubuntu-latest 自带 gcc）；
                      # 本机 Windows 未装 gcc，CGO_ENABLED=0 下 -race 不可用
```

新增测试：`internal/index/store_test.go` 的 `TestFinishUpload_SnapshotGuard` ---- 覆盖"上传期间本地被改，旧快照回写必须被拒绝且新版本仍可被领取"。

### C.5 USN 修复的实测验证

USN 后端此前的失败是**静默降级**（日志一行 `[!] usn 后端不可用 ... 回退 poll`），因此这次用真实文件操作做了端到端验证，而不只依赖单测：

```
场景                       修复前                    修复后
---------------------------------------------------------------------------
启动元数据查询              ERROR_INVALID_USER_BUFFER 正常（JournalID 可读）
创建文件                    poll 兜底检出              usn 检出（新增 1）
修改文件                    poll 兜底检出              usn 检出（修改 1）
删除文件（进回收站）        索引残留 state=LOCAL_NEW   删除 1 且落库 state=REMOVED_LOCAL
新增目录 + 文件             poll 兜底                  新增 2 项
改名                        poll 兜底                  无旧路径残留，renamed_from 正常
```

判定依据直接查索引库（`select rel_path, state from entry`），不只看控制台文案：

| | 修复前 | 修复后 |
|---|---|---|
| 删除后的行状态 | `('d.txt', 2)` 残留 | `('d.txt', 9)` 正确 |

另外确认了 `idx_pending_order` / `idx_history` / `idx_blocked` 三条新索引在真实建库时确实创建（`sqlite_master` 查得）。

