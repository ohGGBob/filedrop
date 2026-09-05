package com.ohggbob.filedrop

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.os.Build
import android.os.IBinder

/**
 * FileDropService：前台服务，让「手机当主机」在锁屏 / 切后台后继续活着。
 *
 * 没有它，Android 会在 App 退到后台几分钟后杀掉整个进程——Go 服务端跟着一起没，
 * 电脑 / 平板上打开的页面突然就连不上了，这是「手机当主机」能不能天天用的命门。
 * 常驻通知是前台服务的代价：系统要让你知道有个东西在共享你的文件。
 */
class FileDropService : Service() {

    companion object {
        const val CHANNEL_ID = "filedrop_service"
        const val NOTIF_ID = 1
        const val EXTRA_URL = "url"
        const val ACTION_STOP = "com.ohggbob.filedrop.action.STOP"
    }

    override fun onCreate() {
        super.onCreate()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val ch = NotificationChannel(
                CHANNEL_ID, "FileDrop 共享状态", NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "FileDrop 正在局域网共享文件时显示"
                setShowBadge(false)
            }
            (getSystemService(NOTIFICATION_SERVICE) as NotificationManager).createNotificationChannel(ch)
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
            return START_NOT_STICKY
        }
        val url = intent?.getStringExtra(EXTRA_URL) ?: ""
        startForeground(NOTIF_ID, buildNotification(url))
        // START_STICKY：万一进程还是被杀（极端内存压力），系统会尝试拉起服务；
        // 服务起来后 Go 服务端需要重新 start，由 MainActivity 的 onNewIntent /
        // 重新打开 App 处理，这里不自行重启 Go 端。
        return START_STICKY
    }

    private fun buildNotification(url: String): Notification {
        val text = if (url.isBlank()) "FileDrop 服务运行中"
        else "同 WiFi 设备可通过下方地址连接 · $url"
        val b = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
            Notification.Builder(this, CHANNEL_ID)
        else
            @Suppress("DEPRECATION") Notification.Builder(this)
        return b
            .setSmallIcon(android.R.drawable.ic_menu_share)
            .setContentTitle("FileDrop 正在共享")
            .setContentText(text)
            .setOngoing(true)
            .build()
    }

    override fun onBind(intent: Intent?): IBinder? = null
}
