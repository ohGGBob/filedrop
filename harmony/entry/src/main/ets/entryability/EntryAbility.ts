import UIAbility from '@ohos.app.ability.UIAbility';
import window from '@ohos.window';
import hilog from '@ohos.hilog';

// Go 侧通过 NDK 导出 Start(port, dir) -> url，与 Android 共用 mobile/mobile.go
// 此处为占位，实际通过 NAPI 引入 libfiledrop.so
// import filedrop from 'libfiledrop.so';

export default class EntryAbility extends UIAbility {
  onCreate(want, launchParam) {
    hilog.info(0x0000, 'FileDrop', 'EntryAbility onCreate');
    // 1) 取应用 filesDir 作为接收目录（鸿蒙沙盒内可写）
    // const dir = this.context.filesDir + '/FileDrop';
    // 2) 启动 Go 服务：filedrop.Start(28080, dir)
    // 3) 拉起前台通知（鸿蒙延续 Android 前台服务语义）
  }
  onWindowStageCreate(windowStage: window.WindowStage) {
    windowStage.loadContent('pages/Index', (err) => {
      if (err.code) { hilog.error(0x0000, 'FileDrop', 'loadContent failed %{public}s', JSON.stringify(err)); return; }
      hilog.info(0x0000, 'FileDrop', 'loadContent succeeded');
    });
  }
}
