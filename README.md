# FileDrop · 局域网文件快传

在**同一 WiFi / 局域网**下，电脑做中心，手机 / 平板用浏览器**零安装**互传文件。
专为个人 1–4GB 级别大文件设计：自动分块、断点续传、SHA-256 校验、配对令牌防误传。

## 适用场景
- 手机 → 电脑、电脑 → 手机、手机 → 平板（都以电脑为中转枢纽）
- 设备基本在同一局域网；跨网场景暂需额外打洞/中继（后续做）

## 目录结构
```
filedrop/
├── main.go            # 无界面命令行入口（薄壳）
├── core/
│   ├── server.go      # 服务端核心（路由 / 下载 / 文件管理 / 二维码）
│   ├── upload.go      # 分块上传：位图断点续传、残留隔离与清理
│   ├── events.go      # SSE 事件推送（多设备实时刷新）
│   ├── settings.go    # 接收目录持久化、打开/选择文件夹
│   └── web/           # 前端（原生 JS），通过 go:embed 编进二进制
├── tray/
│   └── main.go        # Windows 托盘入口（自包含，内嵌服务端）
├── testclient/        # 自测客户端（分块协议参考实现）
├── received/          # 接收文件落盘目录（运行时在 exe 旁自动创建）
├── filedrop.exe       # 单文件成品（前端已内嵌，拷到哪都能跑）
├── filedrop-tray.exe  # 单文件托盘版
├── go.mod / go.sum
└── PROTOCOL.md        # 前后端传输协议契约
```

> **单文件成品**：前端通过 `go:embed` 编译进二进制，两个 exe 都不依赖外部的 `web/` 目录，
> 单独拷到任意空文件夹即可运行（`received/` 会在 exe 同级自动建）。已实测验证。

## 运行（Windows）
两种启动方式：
- **托盘版（推荐）**：双击 `filedrop-tray.exe`，后台拉起无界面服务，自动打开传输页面；任务栏托盘菜单可「打开传输页面 / 退出」。
- **无界面版**：双击 `filedrop.exe`（已编译好，零依赖），自己开浏览器访问控制台打印的地址。

或自行编译：
```bash
go build -o filedrop.exe .
go build -o filedrop-tray.exe ./tray
./filedrop.exe
```
可选参数：
```
-port 28080    监听端口（默认 28080）
-dir  received 接收目录
-no-auth       关闭配对令牌（仅本地测试用）
```
> 启动后网页会显示本机连接地址的**二维码**，手机 / 平板扫一扫即可打开（已含上传令牌），免输地址。
>
> 首次自行构建需联网拉取两个依赖（`skip2/go-qrcode`、`getlantern/systray`）；国内建议先设 `GOPROXY=https://goproxy.cn,direct GOSUMDB=off`。
启动后控制台会打印「本机连接地址」，形如：
`http://192.168.1.20:28080/?t=xxxx`

## 使用
1. **电脑端**：打开 `http://localhost:28080/`（或控制台地址），页面显示「本机连接地址」。
2. **手机 / 平板**：在浏览器里打开控制台打印的带令牌地址（复制粘贴或后续扫码）。
3. **发送（上传）**：在手机/电脑页面点「发送」选文件（**可多选**）→ 自动排队分块上传，显示进度/速度/ETA；中断后可再次选同一文件续传。
4. **接收（下载）**：页面列出电脑端 `received/` 里的文件（**新的在最上面**），点「下载」即用浏览器原生保存（支持断点续传）。
5. **清理中断**：上传中途关页面/断网会留下未传完的临时数据（可能几 GB）。页面「中断的传输」区块可列出并清理，已传完的文件不受影响。

> 注意：上传需要带令牌的地址（防同 WiFi 其他人乱传）；下载/列表不需要。

## 协议简述
详见 `PROTOCOL.md`（v0.3）。要点：
- 分块 8MiB，三段式：`status` 查缺块 → `POST /api/upload/chunk` 按偏移 `WriteAt` 落盘 → `complete` 收尾
- `complete` 阶段校验位图、算 SHA-256、重命名落盘；缺块返回 400 + `missing` 触发客户端续传
- 下载 `GET /api/download?name=` 走 `http.ServeContent`，自动支持 `Range`/206
- **残留文件命名必须带 size**：`<name>.<size>.part[.bits]`
  否则同名不同大小的两次上传会共用位图，导致文件被静默写坏且 SHA 校验发现不了
- 下载响应头按 RFC 6266 双写 `filename` + `filename*=UTF-8''...`，中文文件名不乱码
- `GET /api/events` 是 SSE 流，任一设备变动都会推送，所有页面实时同步列表

## 自测
```bash
# 起服务
./filedrop.exe -port 28099 &
# 取令牌
TOKEN=$(curl -s http://127.0.0.1:28099/api/info | python -c "import sys,json;print(json.load(sys.stdin)['url'].split('t=')[1])")
# 造 4GB 随机文件并跑 上传→下载回环（含 SHA 校验）
dd if=/dev/urandom of=big.bin bs=1M count=4096
go run ./testclient up http://127.0.0.1:28099 "$TOKEN" big.bin
```

## 已知限制
- **iOS Safari** 对大文件（`File.slice` 上传）限制较多；当前目标为 Android，iOS 待适配。
- 配对令牌是**弱防护**（同网设备拿到页面即可读取），仅防误传，非安全边界。
- 暂不支持设备间直接 P2P（手机↔平板需经电脑中转）；跨网需另加中继/穿透。
- 单文件受接收目录所在文件系统限制（NTFS/exFAT 均可 >4GB）。

## 后续路线
- **P2（待真机实测）**：安卓真机扫码互传。Windows 端已验证：4GB 分块上传、断点续传、SHA-256、二维码、托盘。
  真机只需手机连同一 WiFi → 扫二维码 → 传一个 4GB 文件看实际速度。
- **P3（已完成）**：二维码（PC 页显示供扫）+ 托盘程序（`filedrop-tray.exe`）
- **P4（已完成）**：单文件成品化——前端 `go:embed` 内嵌，两个 exe 自包含；托盘版内嵌服务端，不再外挂子进程
- **P5（已完成，v0.3.0）**：稳定性与体验打磨
  - **修复 Windows 上前端 JS 从未加载的致命 bug**：`filepath.Clean` 在 Windows 会把 `/app.js`
    清成 `\app.js`，embed.FS 打开失败，静态资源被 SPA 回退吞成首页——页面只剩 HTML，
    所有按钮 / 列表 / 上传交互全部失效。改用 `path.Clean`（URL 恒为正斜杠）修复，
    并让带扩展名的未知路径返回 404 而不是悄悄回退
  - 修复「同名不同 size」残留串味导致的**静默数据损坏**（size 编进 `.part` 文件名）
  - 修复中文文件名下载乱码（RFC 6266 双写 `Content-Disposition`）
  - 多文件批量上传（可多选 / 拖多个，串行排队 + 整体进度）
  - 中断残留可视化与清理（新增 `GET/DELETE /api/uploads` + 页面「中断的传输」区块）
  - 文件列表按修改时间倒序
- **明确不做**：**开机自启**（按尤利乌斯要求，保持按需手动启动，避免服务常驻监听）
- iOS Safari 大文件上传限制较多，暂不在目标内

## 自测覆盖（v0.3.0 已跑通）
`testclient` 回环（上传 → 下载 → SHA 比对）之外，还验证了：
- 同名不同 size 隔离：先中断 24MB 的 `dup.bin`，再传同名 40MB 版本 → 报告「缺 5 块」（未被旧位图污染），SHA 与回环均一致
- 中文文件名：`Content-Disposition` 正确输出 `filename*=UTF-8''...`
- 乱序断点续传：只传中间块后，`status` 精确返回 `missing:[0,2]`，续传只补 2 块
- 残留清理：列出 → 按名清理 → 清空，已接收文件不受影响

### 关于开机自启
本项目**不做**开机自启 / 常驻后台。需要传文件时手动双击 `filedrop-tray.exe`，
用完从托盘菜单「退出」即可。这样服务不会长期挂在局域网上，干净也更省心。
