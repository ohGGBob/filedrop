package com.ohggbob.filedrop

import android.os.ParcelFileDescriptor
import org.json.JSONObject
import java.io.DataOutputStream
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder

/**
 * Uploader：在 Kotlin 侧直接走 FileDrop 的分块上传协议（status → chunk → complete）。
 *
 * 为什么不交给网页里的 JS：系统分享进来的是 content:// Uri，WebView 里的 JS
 * 摸不到它的字节流。这里用 ParcelFileDescriptor 定位读取，8MiB 一块地喂给
 * 服务端——协议与网页 / testclient 完全一致，天然支持断点续传。
 */
object Uploader {

    private const val CHUNK = 8 * 1024 * 1024

    /** 解析对端页面地址（http://ip:port/?t=xxx 或 ?g=xxx）→ (base, 凭据) */
    fun splitPeer(url: String): Pair<String, String>? {
        val m = Regex("""^(http://[^/]+)/\?(?:t|g)=([0-9a-fA-F]+)$""").find(url.trim()) ?: return null
        return m.groupValues[1] to m.groupValues[2]
    }

    private fun enc(s: String) = URLEncoder.encode(s, "UTF-8")

    private fun getJSON(url: String): JSONObject? = try {
        val c = URL(url).openConnection() as HttpURLConnection
        c.connectTimeout = 5000
        c.readTimeout = 15000
        c.requestMethod = "GET"
        if (c.responseCode / 100 != 2) { c.disconnect(); null }
        else c.inputStream.bufferedReader().use { try { JSONObject(it.readText()) } catch (_: Exception) { null } }
            .also { c.disconnect() }
    } catch (_: Exception) { null }

    private fun post(url: String, body: ByteArray?): Int = try {
        val c = URL(url).openConnection() as HttpURLConnection
        c.connectTimeout = 5000
        c.readTimeout = 120000
        c.requestMethod = "POST"
        if (body != null && body.isNotEmpty()) {
            c.doOutput = true
            c.setRequestProperty("Content-Type", "application/octet-stream")
            c.setFixedLengthStreamingMode(body.size)
            DataOutputStream(c.outputStream).use { it.write(body) }
        }
        val code = c.responseCode
        c.inputStream?.use { it.readBytes() }
        c.disconnect()
        code
    } catch (_: Exception) { -1 }

    /** 顺序读完 [InputStream] 的前 off 字节后读出 len 字节（配合重开的流做“定位读”）。 */
    private fun readChunk(open: () -> InputStream, off: Long, len: Int, buf: ByteArray): Int? {
        val ins = open()
        return try {
            var skipped = 0L
            while (skipped < off) {
                val n = ins.skip(off - skipped)
                if (n <= 0) return null // 流不支持定位 / 已到头
                skipped += n
            }
            var filled = 0
            while (filled < len) {
                val n = ins.read(buf, filled, len - filled)
                if (n < 0) break
                filled += n
            }
            filled
        } finally {
            ins.close()
        }
    }

    /**
     * 上传一个文件。
     * @param open 每次需要重新定位时打开新流（content:// 的流只能顺序读，用 PFD 可重复打开）
     * @return (最终文件名, 是否被对端判为重复跳过)；失败抛异常。
     */
    fun upload(
        base: String, cred: String, name: String, size: Long,
        open: () -> InputStream,
        onProgress: (sentChunks: Int, totalChunks: Int) -> Unit,
    ): Pair<String, Boolean> {
        // 统一的接口地址拼接：凭据在前，业务参数在后
        val credPart = if (cred.isBlank()) "" else "t=$cred&"
        fun qs(extra: String) = "$base/api/upload?$credPart$extra"

        // 1) 查缺块（对端同内容已存在时会直接 complete）
        val st = getJSON(qs("name=${enc(name)}&size=$size"))
            ?: throw IllegalStateException("对端无响应（status）")
        if (st.optBoolean("complete")) return (st.optString("name", name)) to true
        val missing = st.optJSONArray("missing")
            ?: throw IllegalStateException("对端响应异常（missing）")
        val total = st.optInt("total", ((size + CHUNK - 1) / CHUNK).toInt())

        // 2) 补缺块
        val buf = ByteArray(CHUNK)
        var sent = 0
        for (i in 0 until missing.length()) {
            val idx = missing.getInt(i)
            val off = idx.toLong() * CHUNK
            val len = minOf(CHUNK.toLong(), size - off).toInt()
            if (len <= 0) continue
            val n = readChunk(open, off, len, buf)
                ?: throw IllegalStateException("读取文件分块失败（offset=$off）")
            val code = post(qs("name=${enc(name)}&index=$idx&size=$size"), buf.copyOf(n))
            if (code !in 200..299) throw IllegalStateException("分块上传失败（HTTP $code）")
            sent++
            onProgress(sent, missing.length())
        }

        // 3) 收尾（同名不同内容时对端自动改名共存）
        for (attempt in 0 until 3) {
            val code = post(qs("name=${enc(name)}&size=$size"), ByteArray(0))
            if (code in 200..299) return name to false
            // 400+missing 的情况：补传后重试一轮（并发窗口小，简单处理即可）
            val again = getJSON(qs("name=${enc(name)}&size=$size"))
            val m2 = again?.optJSONArray("missing")
            if (m2 != null && m2.length() > 0) {
                for (k in 0 until m2.length()) {
                    val idx = m2.getInt(k)
                    val off = idx.toLong() * CHUNK
                    val l2 = minOf(CHUNK.toLong(), size - off).toInt()
                    if (l2 <= 0) continue
                    val n = readChunk(open, off, l2, buf)
                        ?: throw IllegalStateException("读取文件分块失败（offset=$off）")
                    post(qs("name=${enc(name)}&index=$idx&size=$size"), buf.copyOf(n))
                }
                continue
            }
            if (attempt == 2) throw IllegalStateException("收尾失败（HTTP $code）")
        }
        throw IllegalStateException("收尾重试次数用尽")
    }

    /** PFD 定位读辅助：为分享的 Uri 生成「每次从 off 开始读」的 open 工厂。 */
    fun pfdOpener(pfd: ParcelFileDescriptor): () -> InputStream {
        return {
            ParcelFileDescriptor.AutoCloseInputStream(
                ParcelFileDescriptor.dup(pfd.fileDescriptor)
            )
        }
    }
}
