#!/bin/bash
set -e
# FileDrop macOS 打包：生成 FileDrop.app + DMG 占位
APP="dist/FileDrop.app"
rm -rf dist
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
echo "Building filedrop-tray for darwin..."
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$APP/Contents/MacOS/filedrop-tray" ./tray
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$APP/Contents/MacOS/filedrop-tray-arm64" ./tray || true
# lipo 合并（若本机为 macOS）
if command -v lipo >/dev/null 2>&1 && [ -f "$APP/Contents/MacOS/filedrop-tray-arm64" ]; then
  lipo -create "$APP/Contents/MacOS/filedrop-tray" "$APP/Contents/MacOS/filedrop-tray-arm64" -output "$APP/Contents/MacOS/FileDrop"
  rm "$APP/Contents/MacOS/filedrop-tray" "$APP/Contents/MacOS/filedrop-tray-arm64"
else
  mv "$APP/Contents/MacOS/filedrop-tray" "$APP/Contents/MacOS/FileDrop"
fi
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>FileDrop</string>
  <key>CFBundleDisplayName</key><string>FileDrop</string>
  <key>CFBundleIdentifier</key><string>com.ohggbob.filedrop</string>
  <key>CFBundleVersion</key><string>1.0.0</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
echo "App built at $APP — 可用 create-dmg 或 hdiutil 打 DMG"
ls -lh "$APP/Contents/MacOS/FileDrop" || true
