package com.ohggbob.filedrop

import android.app.DownloadManager
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.net.wifi.WifiManager
import android.os.Bundle
import android.os.Environment
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
import mobile.Mobile
import java.net.URLDecoder

class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "FileDrop"
        private const val PORT = 28080L
        private const val FILE_CHOOSER_REQ = 4201
    }

    private lateinit var webView: WebView
    private var filePathCallback: ValueCallback<Array<Uri>>? = null
    private var multicastLock: WifiManager.MulticastLock? = null

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

        // App 私有外部目录存接收文件；网页里还能随时改目录
        val recvDir = getExternalFilesDir(null)?.absolutePath ?: filesDir.absolutePath

        // Go 服务端在后台线程启动，避免主线程 ANR
        var startError: String? = null
        var pageUrl: String? = null
        val t = Thread {
            try {
                pageUrl = Mobile.start(PORT, recvDir)
            } catch (e: Throwable) {
                Log.e(TAG, "服务启动失败", e)
                startError = e.message ?: e.toString()
            }
        }
        t.start()
        t.join(5000)

        if (startError != null || pageUrl == null) {
            Toast.makeText(this, "服务启动失败：${startError ?: "超时"}", Toast.LENGTH_LONG).show()
            finish()
            return
        }

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

        // 返回键：WebView 先退，退无可退才退出 App
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                if (this@MainActivity::webView.isInitialized && webView.canGoBack()) webView.goBack()
                else finish()
            }
        })

        webView.loadUrl(pageUrl!!)
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == FILE_CHOOSER_REQ) {
            val cb = filePathCallback
            filePathCallback = null
            cb?.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(resultCode, data))
        }
    }

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

    override fun onDestroy() {
        try { Mobile.stop() } catch (_: Throwable) {}
        try { multicastLock?.release() } catch (_: Throwable) {}
        if (this::webView.isInitialized) webView.destroy()
        super.onDestroy()
    }
}
