package com.ohggbob.filedrop

import android.Manifest
import android.app.DownloadManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.os.ParcelFileDescriptor
import android.provider.OpenableColumns
import android.util.Log
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import androidx.activity.OnBackPressedCallback
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import mobile.Mobile
import java.io.InputStream
import java.net.URLDecoder
import kotlin.concurrent.thread

class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "FileDrop"
        private const val PORT = 28080L
        private const val FILE_CHOOSER_REQ = 4201
        private const val NOTIF_PERM_REQ = 4202
        private const val PROGRESS_CHANNEL = "filedrop_progress"
        private const val PROGRESS_NOTIF_ID = 100
    }

    private lateinit var webView: WebView
    private var filePathCallback: ValueCallback<Array<Uri>>? = null
    private var multicastLock: WifiManager.MulticastLock? = null
    private var pageUrl: String = ""

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Android 默认丢弃广播/组播包，不拿 MulticastLock 就收不到局域网设备发现信标
        try {
            val wifi = applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
            multicastLock = wifi.createMulticastLock("filedrop-mcast").apply {
                setReferenceCounted(false)
                acquire()
            }
        } catch (e: Exception) {
            Log.w(TAG, "MulticastLock 获取失败（设备发现可能不可用）: ${e.message}")
        }

        createProgressChannel()
        requestNotificationPermission()

        // App 私有外部目录存接收文件；网页里还能随时改目录
        val recvDir = getExternalFilesDir(null)?.absolutePath ?: filesDir.absolutePath

        // Go 服务端在后台线程启动，避免主线程 ANR
        var startError: String? = null
        val t = thread {
            try {
                pageUrl = Mobile.start(PORT, recvDir)
            } catch (e: Throwable) {
                Log.e(TAG, "服务启动失败", e)
                startError = e.message ?: e.toString()
            }
        }
        t.join(5000)

        if (startError != null || pageUrl.isBlank()) {
            Toast.makeText(this, "服务启动失败：${startError ?: "超时"}", Toast.LENGTH_LONG).show()
            finish()
            return
        }

        // 前台服务保活：锁屏 / 切后台后 Go 服务端随进程一起被杀，是「手机当主机」的命门
        ContextCompat.startForegroundService(
            this,
            Intent(this, FileDropService::class.java).putExtra(FileDropService.EXTRA_URL, pageUrl)
        )

        webView = WebView(this)
        setContentView(webView)

        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true // 前端用 localStorage 记下载前缀
            cacheMode = WebSettings.LOAD_DEFAULT
            useWideViewPort = true
            loadWithOverviewMode = true
        }

        webView.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                // 只允许本地服务的页面；外链交系统浏览器
                val u = request.url
                return if (u.host == "127.0.0.1" || u.host == "localhost") false
                else {
                    try { startActivity(Intent(Intent.ACTION_VIEW, u)) } catch (_: Exception) {}
                    true
                }
            }
        }

        // 文件选择（手机往电脑/平板发文件的入口）
        webView.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                view: WebView?,
                callback: ValueCallback<Array<Uri>>?,
                params: FileChooserParams?
            ): Boolean {
                filePathCallback?.onReceiveValue(null)
                filePathCallback = callback
                val intent = params?.createIntent()
                    ?: Intent(Intent.ACTION_GET_CONTENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = "*/*"
                    }
                intent.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
                return try {
                    startActivityForResult(intent, FILE_CHOOSER_REQ)
                    true
                } catch (e: Exception) {
                    filePathCallback = null
                    Toast.makeText(this@MainActivity, "无法打开文件选择器", Toast.LENGTH_SHORT).show()
                    false
                }
            }
        }

        // 下载走 DownloadManager，保存到公共下载目录的 FileDrop 子目录
        webView.setDownloadListener { url, _, contentDisposition, mimetype, _ ->
            download(url, contentDisposition, mimetype)
        }

        // 返回键：WebView 先退，退无可退才退出 App（退出时前台服务一并结束）
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                if (this@MainActivity::webView.isInitialized && webView.canGoBack()) webView.goBack()
                else finish()
            }
        })

        webView.loadUrl(pageUrl)

        // 从系统分享进来（相册 → 分享 → FileDrop）
        handleShare(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (this::webView.isInitialized) handleShare(intent)
    }

    // ---------- 系统分享 ----------

    private fun shareUris(intent: Intent?): List<Uri> {
        if (intent == null) return emptyList()
        return when (intent.action) {
            Intent.ACTION_SEND ->
                @Suppress("DEPRECATION")
                listOfNotNull(intent.getParcelableExtra(Intent.EXTRA_STREAM))
            Intent.ACTION_SEND_MULTIPLE ->
                @Suppress("DEPRECATION")
                (intent.getParcelableArrayListExtra<Uri>(Intent.EXTRA_STREAM) ?: arrayListOf())
            else -> emptyList()
        }
    }

    private fun handleShare(intent: Intent?) {
        val uris = shareUris(intent)
        if (uris.isEmpty()) return
        Toast.makeText(this, "正在通过 FileDrop 发送 ${uris.size} 个文件…", Toast.LENGTH_SHORT).show()
        thread(name = "fd-share") {
            try { shareUpload(uris) } catch (e: Exception) {
                Log.e(TAG, "分享发送失败", e)
                runOnUiThread { Toast.makeText(this, "发送失败：${e.message}", Toast.LENGTH_LONG).show() }
            }
        }
    }

    /** 分享发送：优先直传上次连接过的设备；没记住任何设备就落在本机接收目录。 */
    private fun shareUpload(uris: List<Uri>) {
        // 目标 1：上次主动连接过的对端（含凭据，见 /api/remember-peer）
        val peer = Mobile.LastPeer().takeIf { it.isNotBlank() }?.let { Uploader.splitPeer(it) }
        // 目标 2（兜底）：本机服务——文件进接收目录，电脑打开手机页面即可取走
        val local = Uploader.splitPeer(pageUrl)

        val (base, cred, targetLabel) =
            peer?.let { Triple(it.first, it.second, "上次连接的设备") }
                ?: local?.let { Triple(it.first, it.second, "本机接收目录") }
                ?: throw IllegalStateException("服务尚未就绪")

        var done = 0
        for (uri in uris) {
            val meta = queryMeta(uri)
            if (meta == null) { done++; continue }
            done++
            updateProgressNotif("($done/${uris.size}) ${meta.name} → $targetLabel", 0, 0, true)
            val pfd = contentResolver.openFileDescriptor(uri, "r")
            if (pfd == null) { updateProgressNotif("无法读取：${meta.name}", 0, 0, false); continue }
            try {
                val opener: () -> InputStream = {
                    ParcelFileDescriptor.AutoCloseInputStream(
                        ParcelFileDescriptor.dup(pfd.fileDescriptor)
                    )
                }
                val (_, skipped) = Uploader.upload(
                    base, cred, meta.name, meta.size, opener
                ) { sent, total ->
                    updateProgressNotif("($done/${uris.size}) ${meta.name} → $targetLabel", sent, total, true)
                }
                updateProgressNotif(
                    "${meta.name} ✓ ${if (skipped) "（对端已有相同内容）" else "已发到 $targetLabel"}",
                    1, 1, false
                )
            } catch (e: Exception) {
                updateProgressNotif("${meta.name} 发送失败：${e.message}", 0, 0, false)
            } finally {
                try { pfd.close() } catch (_: Exception) {}
            }
        }
    }

    private data class ShareMeta(val name: String, val size: Long)

    private fun queryMeta(uri: Uri): ShareMeta? {
        contentResolver.query(uri, null, null, null, null)?.use { c ->
            val iName = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
            val iSize = c.getColumnIndex(OpenableColumns.SIZE)
            if (c.moveToFirst() && iName >= 0) {
                val name = c.getString(iName) ?: return null
                val size = if (iSize >= 0 && !c.isNull(iSize)) c.getLong(iSize) else -1L
                if (size <= 0) return null
                return ShareMeta(name, size)
            }
        }
        return null
    }

    // ---------- 通知 ----------

    private fun createProgressChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val ch = NotificationChannel(
                PROGRESS_CHANNEL, "FileDrop 传输进度", NotificationManager.IMPORTANCE_LOW
            ).apply { setShowBadge(false) }
            (getSystemService(NOTIFICATION_SERVICE) as NotificationManager).createNotificationChannel(ch)
        }
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
            != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(
                this, arrayOf(Manifest.permission.POST_NOTIFICATIONS), NOTIF_PERM_REQ
            )
        }
    }

    private fun updateProgressNotif(text: String, done: Int, total: Int, indeterminate: Boolean) {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        val b = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
            Notification.Builder(this, PROGRESS_CHANNEL)
        else
            @Suppress("DEPRECATION") Notification.Builder(this)
        val pi = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val n: Notification = b
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setContentTitle("FileDrop 发送中")
            .setContentText(text)
            .setOngoing(indeterminate || done < total)
            .setContentIntent(pi)
            .setOnlyAlertOnce(true)
            .apply {
                if (!indeterminate) setProgress(total, done, false)
                else setProgress(0, 0, true)
            }
            .build()
        try { nm.notify(PROGRESS_NOTIF_ID, n) } catch (_: SecurityException) {}
    }

    // ---------- 下载 ----------

    private fun download(url: String, contentDisposition: String?, mimetype: String?) {
        try {
            val name = fileNameFrom(contentDisposition, url)
            val req = DownloadManager.Request(Uri.parse(url)).apply {
                setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, "FileDrop/$name")
                mimetype?.takeIf { it.isNotBlank() }?.let { setMimeType(it) }
            }
            (getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager).enqueue(req)
            Toast.makeText(this, "开始下载到 下载/FileDrop/", Toast.LENGTH_SHORT).show()
        } catch (e: Exception) {
            Toast.makeText(this, "下载失败：${e.message}", Toast.LENGTH_LONG).show()
        }
    }

    // 服务端按 RFC 6266 双写文件名：filename*=UTF-8''... 是真实名字，filename="..." 是 ASCII 兜底
    private fun fileNameFrom(disposition: String?, url: String): String {
        if (!disposition.isNullOrBlank()) {
            Regex("filename\\*=UTF-8''([^;]+)", RegexOption.IGNORE_CASE).find(disposition)?.let {
                return URLDecoder.decode(it.groupValues[1].trim(), "UTF-8")
            }
            Regex("filename=\"([^\"]+)\"", RegexOption.IGNORE_CASE).find(disposition)?.let {
                return it.groupValues[1]
            }
        }
        return android.webkit.URLUtil.guessFileName(url, disposition, null)
    }

    // ---------- 生命周期 ----------

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == FILE_CHOOSER_REQ) {
            val cb = filePathCallback
            filePathCallback = null
            cb?.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(resultCode, data))
        }
    }

    override fun onDestroy() {
        // 只有真正退出 App（back 出去）才到这里；Home 键切后台不触发，
        // 服务端继续由前台服务保活。
        try { Mobile.stop() } catch (_: Throwable) {}
        stopService(Intent(this, FileDropService::class.java))
        try { multicastLock?.release() } catch (_: Throwable) {}
        if (this::webView.isInitialized) webView.destroy()
        super.onDestroy()
    }
}
