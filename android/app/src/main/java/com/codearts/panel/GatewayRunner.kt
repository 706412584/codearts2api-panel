package com.codearts.panel

import android.content.Context
import android.util.Log
import org.json.JSONObject
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import java.security.SecureRandom

/**
 * 网关的生命周期与状态管理。
 *
 * 网关以 native 库（`libcodeartsgw.so`，由 `mobile/` 用 `-buildmode=c-shared` 编出）
 * 的形式**跑在 App 进程内**，不再 exec 子进程。原因见 mobile/jni.go：App 自己 spawn
 * 的子进程会被 Android 登记为 phantom process，App 一离开前台就被冻结，监听随之失效；
 * 只有跑在 App 进程里的工作才受前台服务保护（这正是 loomy2api 安卓版靠 Chaquopy 内嵌
 * Python 拿到的效果）。
 *
 * 所有状态仍以文件形式落在 [Context.getFilesDir] 下（config.json / auths / data）。
 */
object GatewayRunner {

    private const val TAG = "GatewayRunner"

    /** 移动端默认端口：与桌面网关的 7866 错开，两者可同时运行。 */
    const val DEFAULT_PORT = 17866

    private const val CONFIG_NAME = "config.json"

    private const val PREFS = "codearts_settings"
    private const val KEY_PORT = "port"
    private const val KEY_ADMIN = "admin_key"
    private const val KEY_API = "api_key"

    /** true 表示本进程内的网关已启动（不代表当前健康，健康看 [probeHealth]）。 */
    @Volatile
    private var started = false

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

    // ------------------------------------------------------------- 生命周期

    fun isRunning(): Boolean = started

    /**
     * 确保网关在跑。幂等：已在运行时直接返回。
     *
     * @return true 表示正在监听；false 表示 native 库不可用或启动失败。
     */
    @Synchronized
    fun ensureRunning(ctx: Context): Boolean {
        if (!GatewayNative.ensureLoaded()) return false
        loadPort(ctx)
        try {
            seedConfig(ctx)
        } catch (t: Throwable) {
            Log.e(TAG, "seed config failed", t)
        }

        // start() 是幂等的：已启动时直接返回 0。传绝对路径——网关以 config 所在目录
        // 解析 ./auths、./data 等相对路径（internal/gateway.resolveDataPaths），
        // 因此不需要（也不应该）改 App 进程的工作目录。
        val cfgPath = File(ctx.filesDir, CONFIG_NAME).absolutePath
        return try {
            val rc = GatewayNative.start(cfgPath)
            if (rc == 0) {
                started = true
                Log.i(TAG, "gateway started port=$port")
                true
            } else {
                started = false
                Log.e(TAG, "gateway start failed rc=$rc")
                false
            }
        } catch (t: Throwable) {
            Log.e(TAG, "failed to start gateway", t)
            false
        }
    }

    /** 停止网关。 */
    @Synchronized
    fun stop() {
        if (!started) return
        try {
            GatewayNative.stop()
        } catch (t: Throwable) {
            Log.w(TAG, "stop gateway failed", t)
        } finally {
            started = false
        }
    }

    /** 重启网关（先停后起）。 */
    fun restart(ctx: Context): Boolean {
        stop()
        return ensureRunning(ctx)
    }

    // ------------------------------------------------------------- 健康检查

    /**
     * 探测 /healthz；网关启动是异步的，UI 需轮询它。
     *
     * 必须显式 Proxy.NO_PROXY：设备设了全局 HTTP 代理（Clash/v2rayNG 等）时，
     * Android 的 HttpURLConnection 会把回环请求也发往代理，代理又够不到手机的
     * 127.0.0.1，表现为 SocketTimeoutException。网关就在本进程内，直连即可。
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
}
