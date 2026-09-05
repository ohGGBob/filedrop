# FileDrop 传输协议契约（v0.4）

前后端约定的 HTTP 接口。所有接口相对当前页面 origin（同源），手机 / 电脑共用同一套前端。

## 通用约定
- 分块大小：`chunkSize = 8 MiB`（8,388,608 字节），客户端与服务端必须一致。
- 配对令牌 `t`：服务端启动时随机生成，拼在连接 URL 上（`?t=xxxx`）。
  **写操作**（上传分块 / 收尾 / 删除 / 重命名 / 改目录 / 清理残留 / 便签）必须带正确令牌，否则 403。
  例外：来自 `127.0.0.1` / `::1` 的请求豁免，方便电脑自己用 localhost 完整控制。
  **读操作**（列表 / 下载 / 查询状态）不强制令牌——**便签除外**，见下文。
- 版本随 `GET /api/info` 的 `version` 字段返回。

### 文件名规则（`safeName`，五道关卡）
客户端只取 basename，服务端再校验一次，任一道不过返回 400 `bad name`：

1. 再次 `filepath.Base` 并抹掉残留的 `/` `\`，拒绝 `.` / `..` / 空 —— 防路径穿越；
2. 拒绝以 `.` 或空格结尾（Windows 资源管理器会隐藏掉，用户以为文件丢了）；
3. 拒绝超过 200 字节的名字；
4. **清洗** Windows 非法字符 `< > : " | ? *` 与控制字符（`<0x20`、`0x7F`）为下划线；
5. 清洗后再判 DOS 保留设备名（`CON` / `PRN` / `AUX` / `NUL` / `COM1`… `LPT9`），命中则拒绝。

第 4 道是必须的，不能只报错了事：这些字符在 macOS / 安卓上合法，直接落盘会撞出两种更糟的结果——
`CreateFile` 报错变成一个看不懂的 500；或 `a:b` 被 NTFS 当成**交替数据流**，接口返回 200、
字节却写进了 `a` 这个文件的数据流里，目录中只留下一个空的 `a`。
清洗会改动文件名，所以 `complete` 会把**真实落盘名**回给前端显示（见下）。

## 上传（三段式：status → chunk → complete）

### 1. 查询断点
`GET /api/upload/status?name=<urlencode>&size=<bytes>&fp=<16hex>`
```json
{ "chunkSize": 8388608, "total": 512, "missing": [0, 3, 7], "received": 25165824, "complete": false }
```
- `missing`：尚未收到的分块下标，客户端只补这些块（支持乱序 / 并发 / 断点续传）。
- `received`：从 0 号块起连续已收的字节数，仅用于显示进度。
- `fp`（**v0.4 新增，强烈建议带**）：客户端对自己那份文件算出的内容取样指纹。
  目录下已存在同名同大小的完整文件、**且 `fp` 与文件实际指纹一致**时，才返回
  `missing: []`、`complete: true`、`name: "<清洗后的真实文件名>"`，客户端可直接跳过上传。
  不带 `fp`、长度不对、或对不上 —— 一律按未完成处理，老老实实重传。

**为什么必须带指纹**：v0.3 之前只判「同名 + 同大小」就返回 `complete: true`。
那正是「改了内容重新导出一份」最容易撞上的组合——文件会被悄悄丢掉，
而界面显示「完成 ✓」，属于最恶劣的一类 bug：说谎。

#### 取样指纹算法（三端必须逐字节一致）
`crypto.subtle` 在明文 http 源上不可用（只有 https 和 loopback 算安全上下文），
所以这里用的是非加密哈希，够用来判「是不是同一份内容」，不承担防伪职责。

- 三个取样窗口：起点 `0`、`size/2 - 32KiB`（不足则取 `size/2`）、`size - 64KiB`（不足则 0），
  每窗口最多 `64 KiB`，短读到的部分**按实际长度参与哈希**（长度进哈希，见下）。
- 双 lane FNV-1a（32 位）：`h1` 初值 `0x811c9dc5`，`h2` 初值 `0x01000193`；
  逐字节 `h ^= b; h *= 0x01000193`。每段数据先混入 `size` 的 8 字节小端表示，
  `h2` 在每段前额外 `h2 ^= len(seg)`（两 lane 不同序，降低碰撞）。
- 输出：`fmt.Sprintf("%08x%08x", h1, h2)`，16 个十六进制字符。

小文件上三窗口会重叠——只要三端用同一套偏移，重叠不影响一致性判断。
实现见 `core/upload.go`（Go）、`core/web/app.js`（JS，`Math.imul` 模拟 32 位乘法）、`testclient/main.go`。

**指纹的盲区**：它只看头 / 中 / 尾共 192 KiB。若新内容总大小不变、且改动恰好全落在三个窗口之外，
会被判成「同一份」而跳过上传。对随机数据这种漏判概率约为 0，对普通文档也几乎必然踩中某处窗口，
但它是**可能**发生的。取舍是「重复传 4GB 的代价」对「这种特定改法的漏判」——
真正的判定仍在 `complete` 的 SHA-256；这里只是让秒跳过尽量不误事。
若某次上传必须强制重传，删掉接收目录里的旧文件（或改名）即可。

### 2. 传分块
`POST /api/upload/chunk?t=<token>&name=<urlencode>&index=<i>&size=<bytes>`
- Body：该分块原始字节，`Content-Type: application/octet-stream`。
- 服务端按 `index * chunkSize` 偏移 `WriteAt` 落盘，并在位图里把该块记 1。
- 末块允许的最大长度是「到文件末尾的余量」，超出返回 400。

### 3. 收尾
`POST /api/upload/complete?t=<token>&name=<urlencode>&size=<bytes>`
- 服务端校验位图已收满；缺块则返回 `400` + `{ "error": "incomplete", "missing": [...] }`，客户端补传后重试。
- 收满则：规整文件长度 → 算 SHA-256 → 挑落盘名 → 写 `<name>.sha256` → 清位图：
```json
{ "sha256": "…", "name": "report (1).docx", "reused": false }
```
  - `name`：**真实落盘名**，可能与客户端传上来的名字不同（被清洗过，或为了避让同名文件而改名），前端必须以此显示。
  - `reused: true`：目录下已有一份 size + SHA-256 完全相同的文件，本次新传的那份 `.part` 已删除，没有落新盘。
  - 同名但内容不同 → **不覆盖**，另存为 `名字 (1).ext`、`名字 (2).ext`…（直接 `os.Rename` 覆盖会让上一份文件无声消失，而手机端通常没有第二份备份）。

### 落盘文件命名（重要）
中断残留的文件名**必须带 size**：

```
<name>.<size>.part        未传完的数据
<name>.<size>.part.bits   接收位图（每块 1 字节，0/1）
```

原因：`size` 不进文件名时，「同名文件、不同大小」的两次上传会共用同一份 `.part` / 位图。
旧位图长度恰好够新文件用时会被直接复用，新文件前 N 块被误判为已收到，
而 complete 算出的 SHA-256 是「新旧拼接后损坏数据」的哈希——**校验会"通过"，损坏被静默写盘**。
带上 size 后两次上传天然隔离。

## 下载
`GET /api/download?name=<urlencode>`
- 走 `http.ServeContent`，自动支持 `Range` / `206` 断点续传。
- 响应头按 RFC 6266 双写，保证中文文件名不乱码：
```
Content-Disposition: attachment; filename="<ascii 兜底>"; filename*=UTF-8''<百分号编码>
```

## 文件管理
| 接口 | 说明 |
| --- | --- |
| `GET /api/files` | 接收目录文件列表，**按修改时间倒序**（含 `name` / `size` / `mtime` / `sha256`），隐藏 `.part` / `.bits` / `.sha256` |
| `DELETE /api/files?name=&t=` | 删除文件，连带 `.sha256` 与该文件的所有 `.part` 残留 |
| `POST /api/rename?t=` | body `{ "name", "newName" }`，重命名并同步迁移 sha256 清单 |

## 中断残留
| 接口 | 说明 |
| --- | --- |
| `GET /api/uploads` | 列出所有中断的上传：`name` / `size` / `partSize` / `chunks` / `have` / `mtime` |
| `DELETE /api/uploads?name=&t=` | 清理残留；**不带 `name` 则清理全部** |

中断的 `.part` 可能是几 GB，且在文件列表里不可见，必须能列出并清理。

## 文本快传（便签）
几十字节的验证码 / 命令 / 地址，走文件那条路太亏（选文件 → 分块 → 落盘 → 再下载）。便签直接存在一个 JSON 里，不占接收目录。

| 接口 | 说明 |
| --- | --- |
| `POST /api/notes?t=` | body 为原始 UTF-8 文本（`Content-Type: text/plain`），返回 `{ id, text, at, size }` |
| `GET /api/notes?t=` | 全部便签，按 `at` 倒序 |
| `DELETE /api/notes?t=&id=` | 删除单条；id 不存在返回 404 |

- **读写一律要令牌**（回环豁免）。这一点与文件不同：便签内容常是验证码、密码、内网地址，
  不像照片那样可以对着局域网敞开读。不带令牌的请求全接口 403。
- 上限：单条 256 KiB、总计 200 条 / 8 MiB，超限时从最旧一条开始丢弃。
- 落盘位置：**exe 同级** `filedrop-notes.json`（刻意放在接收目录之外——用户清接收目录不该顺手删掉历史记录）。
  写 `.tmp` 再 `os.Rename` 原子替换；文件损坏时按空处理而不是让整个接口 500。
- 前端渲染一律用 `textContent`，正文里的 `<img onerror=…>` 不会变成元素（已实测）。

## 其他
| 接口 | 说明 |
| --- | --- |
| `GET /api/info` | `{ ip, port, url, version }` |
| `GET /api/events` | SSE 流：文件变动推 `{"type":"files"}`，便签变动推 `{"type":"notes"}`，多设备实时刷新 |
| `GET /api/qr?text=&size=` | 返回连接地址二维码 PNG |
| `GET/POST /api/settings` | 读取 / 切换接收目录（POST 需令牌），写入 exe 同级 `filedrop-config.json` |
| `POST /api/open-folder` | 在资源管理器打开接收目录 |
| `POST /api/pick-folder` | 弹 Windows 原生文件夹选择对话框（仅 Windows） |

### 切换接收目录的约束
有未传完的 `.part` 时 `POST /api/settings` **拒绝切换**并返回残留条数。
原因不是洁癖：分块请求各自按当时的目录拼 `.part` 路径，切目录会把一次上传的 `.part` 与位图
劈到两个目录下，而「中断的传输」面板只扫当前目录——旧目录里那几 GB 残留就此变成看不见也清不掉的孤儿。

## 前端环境约束（影响实现，勿忽略）
- 明文 `http://192.168.x.x` **不是安全上下文**：`navigator.clipboard`、`crypto.subtle` 都不可用
  （`http://localhost` 例外）。复制必须走 `document.execCommand('copy')` 兜底并准备失败提示，
  判重只能用非加密哈希（见取样指纹）。
- 安卓浏览器的下载目录由浏览器自身决定，网页改不了；前端只提供文件名前缀设置。

## 校验
上传完成时服务端算 SHA-256 并返回；`testclient` 会再下载回来算一次做回环比对。
注意：SHA-256 校验的是「服务端落盘后的文件」，它只能证明传输无误，
**无法**发现因分块串味导致的落盘前损坏——所以落盘命名必须遵守上面的 size 隔离规则。
