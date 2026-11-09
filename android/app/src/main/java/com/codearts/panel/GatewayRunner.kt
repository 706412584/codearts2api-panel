package com.codearts.panel

import android.content.Context
import android.util.Log
import org.json.JSONObject
import java.io.BufferedReader
import java.io.File
import java.io.InputStreamReader
import java.net.HttpURLConnection
import java.net.URL
import java.security.SecureRandom

/**
 * 运行网关子进程（Go 二进制）的进程管理器。
 *
 * 与 loomy2api 的 Python 版不同：这里是 exec 一个 Android arm64 原生二进制，
 * 而不是启动 Chaquopy 解释器，因此没有 Python 的全局状态问题——进程死了重启一个
 * 就是干净的。所有状态都以文件形式落在 [Context.getFilesDir] 下。
 *
 * 二进制放在 APK 的 `jniLibs/arm64-v8a/libcodearts-gw.so`。Android 只允许从
 * `nativeLibraryDir` 执行 App 内的文件（targetSdk 29+ 下 filesDir 受 W^X 限制），
 * 所以必须伪装成 `.so` 并 `useLegacyPackaging = true` 解压安装。
 */
object GatewayRunner {

    private const val TAG = "GatewayRunner"

    /** 移动端默认端口：与桌面网关的 7866 错开，两者可同时运行。 */
    const val DEFAULT_PORT = 17866

    private const val BINARY_NAME = "libcodearts-gw.so"
    private const val CONFIG_NAME = "config.json"
    private const val LOG_NAME = "gateway.log"

    private const val PREFS = "codearts_settings"
    private const val KEY_PORT = "port"
    private const val KEY_ADMIN = "admin_key"
    private const val KEY_API = "api_key"

    @Volatile
    private var process: Process? = null

    /** 已生成密钥的内存缓存，避免每次读盘。 */
    @Volatile
    private var cachedAdminKey: String = ""

    @Volatile
    var port: Int = DEFAULT_PORT
        private set

    // ------------------------------------------------------------- 首选项

    fun loadPort(ctx: Context): Int {
        val stored = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getInt(KEY_PORT, 0)
        port = if (stored in 1024..65535) stored else DEFAULT_PORT
        return port
    }

    fun savePort(ctx: Context, value: Int) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit().putInt(KEY_PORT, value).apply()
        port = value
    }

    /** 面板管理密钥（= config.json 的 admin_key），UI 用它自动登录。 */
    fun adminKey(ctx: Context): String {
        if (cachedAdminKey.isNotEmpty()) return cachedAdminKey
        val fromPrefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getString(KEY_ADMIN, "").orEmpty()
        cachedAdminKey = if (fromPrefs.isNotEmpty()) fromPrefs else readKeyFromConfig(ctx, "admin_key")
        return cachedAdminKey
    }

    /** 模型 API 密钥（= config.json 的 api_key），设置页展示用。 */
    fun apiKey(ctx: Context): String {
        val fromPrefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getString(KEY_API, "").orEmpty()
        return if (fromPrefs.isNotEmpty()) fromPrefs else readKeyFromConfig(ctx, "api_key")
    }

    private fun readKeyFromConfig(ctx: Context, field: String): String = try {
        val f = File(ctx.filesDir, CONFIG_NAME)
        if (f.exists()) JSONObject(f.readText()).optString(field, "") else ""
    } catch (t: Throwable) {
        Log.w(TAG, "read $field from config failed", t); ""
    }

    // ------------------------------------------------------------- 配置种子

    /**
     * 确保 filesDir 下有一份可用的 config.json，并把监听地址固定到当前端口。
     *
     * 网关自带的 ensureConfig 也会生成随机密钥，但那样 App 拿不到明文去注入
     * WebView（面板靠 admin_key 鉴权）。这里由 App 先生成并记住密钥；网关看到
     * 文件已存在就不会覆盖。
     */
    private fun seedConfig(ctx: Context) {
        val f = File(ctx.filesDir, CONFIG_NAME)
        val prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val obj = if (f.exists()) {
            try { JSONObject(f.readText()) } catch (_: Throwable) { JSONObject() }
        } else JSONObject()

        var changed = false
        fun ensureKey(field: String, prefKey: String) {
            if (obj.optString(field).isBlank()) {
                val key = prefs.getString(prefKey, "").orEmpty().ifBlank { randomKey() }
                obj.put(field, key)
                prefs.edit().putString(prefKey, key).apply()
                if (field == "admin_key") cachedAdminKey = key
                changed = true
            } else {
                // 配置里已有（用户手动改过 / 网关生成过）：同步回 prefs 供 UI 使用
                prefs.edit().putString(prefKey, obj.optString(field)).apply()
                if (field == "admin_key") cachedAdminKey = obj.optString(field)
            }
        }
        ensureKey("admin_key", KEY_ADMIN)
        ensureKey("api_key", KEY_API)

        if (obj.optString("listen") != "127.0.0.1:$port") {
            obj.put("listen", "127.0.0.1:$port")
            changed = true
        }
        if (!obj.has("watch")) {
            obj.put("watch", JSONObject().apply {
                put("enabled", true)
                put("poll_minutes", 30)
                put("refresh_skew_minutes", 30)
                put("keepalive_interval_minutes", 15)
            })
            changed = true
        }
        if (!obj.has("auth_dir")) { obj.put("auth_dir", "./auths"); changed = true }
        if (!obj.has("state_file")) { obj.put("state_file", "./data/state.json"); changed = true }
        if (!obj.has("default_model")) { obj.put("default_model", "glm-5.2"); changed = true }

        if (changed || !f.exists()) {
            f.parentFile?.mkdirs()
            f.writeText(obj.toString(2))
        }
    }

    private fun randomKey(): String {
        val b = ByteArray(32)
        SecureRandom().nextBytes(b)
        return b.joinToString("") { "%02x".format(it) }
    }

    // ------------------------------------------------------------- 进程

    private fun binary(ctx: Context): File? {
        val f = File(ctx.applicationInfo.nativeLibraryDir, BINARY_NAME)
        return f.takeIf { it.isFile }
    }

    @Synchronized
    fun isRunning(): Boolean = process?.isAlive == true

    /**
     * 确保网关在跑。幂等：已在运行时直接返回。
     *
     * @return true 表示进程已启动或已在运行；false 表示二进制缺失等硬失败。
     */
    @Synchronized
    fun ensureRunning(ctx: Context): Boolean {
        if (isRunning()) return true
        // 端口已被应答说明已有实例在跑（本进程的子进程，或 App 被杀但子进程侥幸存活的
        // 孤儿）。此时再 spawn 一个会因 bind 失败而 log.Fatalf 退出，形成"起一个死一个"
        // 的抖动循环。先探一次，健康就直接复用。
        if (probeHealth()) {
            Log.i(TAG, "gateway already answering on port=$port; skip spawn")
            return true
        }
        val bin = binary(ctx) ?: run {
            Log.e(TAG, "gateway binary missing in ${ctx.applicationInfo.nativeLibraryDir}")
            return false
        }
        loadPort(ctx)
        try {
            seedConfig(ctx)
        } catch (t: Throwable) {
            Log.e(TAG, "seed config failed", t)
        }

        return try {
            val pb = ProcessBuilder(bin.absolutePath, "-config", CONFIG_NAME)
                .directory(ctx.filesDir)
                .redirectErrorStream(true)
            pb.environment()["HOME"] = ctx.filesDir.absolutePath
            pb.environment()["TMPDIR"] = ctx.cacheDir.absolutePath
            val p = pb.start()
            process = p
            pipeLogs(p, File(ctx.filesDir, LOG_NAME))
            Log.i(TAG, "gateway started port=$port")
            true
        } catch (t: Throwable) {
            Log.e(TAG, "failed to start gateway", t)
            false
        }
    }

    /** 停止网关；先礼貌 destroy，宽限后强杀。 */
    @Synchronized
    fun stop() {
        val p = process ?: return
        process = null
        try {
            p.destroy()
            if (!p.waitFor(3, java.util.concurrent.TimeUnit.SECONDS)) {
                p.destroyForcibly()
            }
        } catch (t: Throwable) {
            Log.w(TAG, "stop gateway failed", t)
        }
    }

    /** 重启网关（先停后起）。 */
    fun restart(ctx: Context): Boolean {
        stop()
        return ensureRunning(ctx)
    }

    private fun pipeLogs(p: Process, logFile: File) {
        Thread({
            try {
                p.inputStream.bufferedReader().use { reader: BufferedReader ->
                    logFile.outputStream().bufferedWriter().use { out ->
                        var line = reader.readLine()
                        while (line != null) {
                            // 网关往 os.Stderr 的直写不经过它自己的 ring 脱敏器
                            // （internal/server/observability.go），这里兜一道，避免
                            // 凭据万一被打出来就落盘到 files/gateway.log。
                            out.write(redact(line)); out.newLine(); out.flush()
                            line = reader.readLine()
                        }
                    }
                }
            } catch (t: Throwable) {
                Log.w(TAG, "log pump ended: ${t.message}")
            }
        }, "gateway-log").apply { isDaemon = true }.start()
    }

    /** 与网关侧 credentialLogPattern 同口径的兜底脱敏。 */
    private fun redact(line: String): String =
        CREDENTIAL_RE.replace(line) { m -> m.groupValues[1] + "\"[REDACTED]\"" }

    private val CREDENTIAL_RE = Regex(
        """(?i)("?(?:access_key_id|secret_access_key|security_token|cloud_dragon_token|refresh_token|access_token|code_verifier|dpop_private_key|authorization|x-auth-token|api_key|admin_key|secret)"?\s*[:=]\s*)[^\r\n]*"""
    )

    // ------------------------------------------------------------- 健康检查

    /**
     * 探测 /healthz；网关启动是异步的，UI 需轮询它。
     *
     * 必须显式 Proxy.NO_PROXY：设备设了全局 HTTP 代理（Clash/v2rayNG 等）时，
     * Android 的 HttpURLConnection 会把回环请求也发往代理，代理又够不到手机的
     * 127.0.0.1，表现为 SocketTimeoutException。网关就在本进程旁，直连即可。
     */
    fun probeHealth(timeoutMs: Int = 1500): Boolean = try {
        val conn = URL("http://127.0.0.1:$port/healthz")
            .openConnection(java.net.Proxy.NO_PROXY) as HttpURLConnection
        conn.connectTimeout = timeoutMs
        conn.readTimeout = timeoutMs
        val ok = conn.responseCode == 200
        conn.disconnect()
        ok
    } catch (t: Throwable) {
        Log.w(TAG, "probeHealth failed: ${t.javaClass.simpleName}: ${t.message}")
        false
    }

    fun baseUrl(): String = "http://127.0.0.1:$port"

    fun logFile(ctx: Context): File = File(ctx.filesDir, LOG_NAME)
}
