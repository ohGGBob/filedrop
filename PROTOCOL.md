# FileDrop 传输协议契约（v0.3）

前后端约定的 HTTP 接口。所有接口相对当前页面 origin（同源），手机 / 电脑共用同一套前端。

## 通用约定
- 分块大小：`chunkSize = 8 MiB`（8,388,608 字节），客户端与服务端必须一致。
- 文件名只取 basename，禁止路径穿越；服务端再次校验（`safeName`）。
- 配对令牌 `t`：服务端启动时随机生成，拼在连接 URL 上（`?t=xxxx`）。
  **写操作**（上传分块 / 收尾 / 删除 / 重命名 / 改目录 / 清理残留）必须带正确令牌，否则 403。
  例外：来自 `127.0.0.1` / `::1` 的请求豁免，方便电脑自己用 localhost 完整控制。
  **读操作**（列表 / 下载 / 查询状态）不强制令牌。
- 版本随 `GET /api/info` 的 `version` 字段返回。

## 上传（三段式：status → chunk → complete）

### 1. 查询断点
`GET /api/upload/status?name=<urlencode>&size=<bytes>`
```json
{ "chunkSize": 8388608, "total": 512, "missing": [0, 3, 7], "received": 25165824, "complete": false }
```
- `missing`：尚未收到的分块下标，客户端只补这些块（支持乱序 / 并发 / 断点续传）。
- 同名文件已存在且大小一致 → `missing: []`、`complete: true`，客户端可直接跳过。

### 2. 传分块
`POST /api/upload/chunk?t=<token>&name=<urlencode>&index=<i>&size=<bytes>`
- Body：该分块原始字节，`Content-Type: application/octet-stream`。
- 服务端按 `index * chunkSize` 偏移 `WriteAt` 落盘，并在位图里把该块记 1。
- 末块允许的最大长度是「到文件末尾的余量」，超出返回 400。

### 3. 收尾
`POST /api/upload/complete?t=<token>&name=<urlencode>&size=<bytes>`
- 服务端校验位图已收满；缺块则返回 `400` + `{ "error": "incomplete", "missing": [...] }`，客户端补传后重试。
- 收满则：规整文件长度 → 算 SHA-256 → 重命名落盘 → 写 `<name>.sha256` → 清位图 → 返回 `{ "sha256": "..." }`。

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
| `GET /api/files` | 接收目录文件列表，**按修改时间倒序**（含 `name` / `size` / `mtime` / `sha256`） |
| `DELETE /api/files?name=&t=` | 删除文件，连带 `.sha256` 与该文件的所有 `.part` 残留 |
| `POST /api/rename?t=` | body `{ "name", "newName" }`，重命名并同步迁移 sha256 清单 |

## 中断残留
| 接口 | 说明 |
| --- | --- |
| `GET /api/uploads` | 列出所有中断的上传：`name` / `size` / `partSize` / `chunks` / `have` / `mtime` |
| `DELETE /api/uploads?name=&t=` | 清理残留；**不带 `name` 则清理全部** |

中断的 `.part` 可能是几 GB，且在文件列表里不可见，必须能列出并清理。

## 其他
| 接口 | 说明 |
| --- | --- |
| `GET /api/info` | `{ ip, port, url, version }` |
| `GET /api/events` | SSE 流，文件变动时推 `{"type":"files"}`，实现多设备实时刷新 |
| `GET /api/qr?text=&size=` | 返回连接地址二维码 PNG |
| `GET/POST /api/settings` | 读取 / 切换接收目录（POST 需令牌），写入 exe 同级 `filedrop-config.json` |
| `POST /api/open-folder` | 在资源管理器打开接收目录 |
| `POST /api/pick-folder` | 弹 Windows 原生文件夹选择对话框（仅 Windows） |

## 校验
上传完成时服务端算 SHA-256 并返回；`testclient` 会再下载回来算一次做回环比对。
注意：SHA-256 校验的是「服务端落盘后的文件」，它只能证明传输无误，
**无法**发现因分块串味导致的落盘前损坏——所以落盘命名必须遵守上面的 size 隔离规则。
