# FileDrop HarmonyOS 壳（鸿蒙 NEXT）

> 复用 `core` + `mobile/mobile.go` 的 Go 服务端（via NDK `libfiledrop.so`），ArkTS 侧仅做 WebView 容器与系统文件桥接，与 Android 共协议。

## 结构
```
harmony/
├── AppScope/app.json5
├── build-profile.json5
├── entry/
│   ├── build-profile.json5
│   └── src/main/ets/
│       ├── entryability/EntryAbility.ts  # 启动 Go 服务 + 拉起前台通知
│       └── pages/Index.ets               # WebView 加载 http://127.0.0.1:28080
```

## 构建（需 DevEco Studio + Harmony SDK + NDK）
```bash
# 1) 编译 Go 为 Harmony NDK so（arm64）
CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=aarch64-linux-ohos-clang go build -buildmode=c-shared -o harmony/entry/libs/arm64/libfiledrop.so ./mobile
# 2) 用 DevEco 打开 harmony/ 目录，Run 'entry'
```

## 与 Android 共用
- 协议完全一致（status→chunk→complete、指纹、临时授权）
- 下载目录：`context.filesDir + "/FileDrop"`，可被「文件管理」浏览
- 分享：`@ohos.file.picker` 选文件后走同一分块上传

后续可接入 `HMOS` 的分布式文件系统与近场发现替代 UDP 广播。
