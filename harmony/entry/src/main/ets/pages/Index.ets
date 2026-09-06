import webview from '@ohos.web.webview';
import filePicker from '@ohos.file.picker';

@Entry
@Component
struct Index {
  controller: webview.WebviewController = new webview.WebviewController();
  @State pageUrl: string = 'http://127.0.0.1:28080/';

  build() {
    Column() {
      // 顶部提示条（适配刘海安全区）
      Row() {
        Text('FileDrop · 鸿蒙版 — 同 WiFi 扫码即传').fontSize(12).fontColor('#64748b')
      }.padding({ top: 8, bottom: 6 }).width('100%').justifyContent(FlexAlign.Center)

      Web({ src: this.pageUrl, controller: this.controller })
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .fileAccess(true)
        .onInterceptRequest((event) => {
          // 拦截外链，交系统浏览器
          const url = event.request.getRequestUrl();
          if (url.startsWith('http://127.0.0.1') || url.startsWith('http://localhost')) return null;
          return null;
        })
        .onShowFileSelector((event) => {
          // 鸿蒙文件选择器桥接：让网页 <input type=file> 能选到图库/文件管理
          const picker = new filePicker.DocumentViewPicker();
          picker.select().then((uris) => {
            event.result.handleFileList(uris);
          }).catch(() => event.result.handleFileList([]));
          return true;
        })
        .layoutWeight(1)
    }
    .padding({ top: 'env(safe-area-inset-top)', bottom: 'env(safe-area-inset-bottom)' })
    .backgroundColor('#f6f7f9')
    .height('100%').width('100%')
  }
}
