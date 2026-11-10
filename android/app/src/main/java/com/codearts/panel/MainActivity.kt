package com.codearts.panel

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.util.Log
import android.webkit.ConsoleMessage
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import org.json.JSONObject
import java.util.concurrent.Executors

/**
 * 主界面：用 WebView 加载网关自带的管理面板 `/panel`。
 *
 * 复用面板（Go 二进制内嵌的响应式网页）而不是重写原生 UI：面板已适配窄屏
 * （`@media(max-width:760px)`），功能与桌面端一致，改一处两端都生效。
 *
 * WebView 默认缺两块能力，这里补齐（否则手机上面板「添加账号」不可用）：
 *  1. `<input type="file">` 需要 [WebChromeClient.onShowFileChooser] 才会弹系统选择器
 *     —— 面板的「导入 JSON」用它选凭据文件。
 *  2. 面板的 OAuth 授权链接是 `<a target="_blank">`，WebView 默认静默丢弃新窗口请求，
 *     且外部站点也不该在面板里打开。这里把非本机地址一律转发给系统浏览器。
 */
class MainActivity : Activity() {

    private lateinit var webView: WebView
    private val ui = Handler(Looper.getMainLooper())
    private val probe = Executors.newSingleThreadExecutor()

    /** 面板的「导入 JSON / 选择文件」回调句柄。 */
    private var filePathCallback: ValueCallback<Array<Uri>>? = null

    /** 密钥只注入一次：赋到 sessionStorage 后整段会话有效。 */
    private var injected = false

    @Volatile
    private var destroyed = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (BuildConfig.DEBUG) WebView.setWebContentsDebuggingEnabled(true)

        webView = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true          // sessionStorage / localStorage 需要
            settings.allowFileAccess = false
            settings.allowContentAccess = false
            // 面板的授权链接是 target="_blank"：不开多窗口的话点击会被丢弃。
            settings.setSupportMultipleWindows(true)
            settings.javaScriptCanOpenWindowsAutomatically = true

            webViewClient = object : WebViewClient() {
                override fun onPageFinished(view: WebView, url: String) {
                    if (destroyed || injected) return
                    if (!url.startsWith(GatewayRunner.baseUrl())) return
                    val key = GatewayRunner.adminKey(applicationContext)
                    if (key.isEmpty()) return
                    injected = true
                    // 通过 evaluateJavascript 注入：面板响应头带 CSP `script-src 'self'`，
                    // 内联脚本会被拦截，而 evaluateJavascript 走页面上下文不受此限。
                    // 赋完 sessionStorage 立即 reload，让 panel.js 读到密钥、跳过登录框。
                    val js = "try{sessionStorage.setItem('codearts2api-panel.admin-key'," +
                        JSONObject.quote(key) + ");location.reload();}catch(e){}"
                    view.evaluateJavascript(js, null)
                }

                override fun shouldOverrideUrlLoading(
                    view: WebView, request: WebResourceRequest
                ): Boolean = handleExternal(request.url)
            }

            webChromeClient = object : WebChromeClient() {
                // 面板「导入 JSON」用 <input type="file">：不实现这里就点不开选择器。
                override fun onShowFileChooser(
                    view: WebView,
                    callback: ValueCallback<Array<Uri>>,
                    params: FileChooserParams
                ): Boolean {
                    filePathCallback?.onReceiveValue(null)
                    filePathCallback = callback
                    return try {
                        startActivityForResult(params.createIntent(), FILE_CHOOSER_REQUEST)
                        true
                    } catch (t: Throwable) {
                        Log.e(TAG, "file chooser unavailable", t)
                        filePathCallback = null
                        false
                    }
                }

                // 面板唯一会开新窗口的地方是 target="_blank" 的 OAuth 授权链接。
                // 弹出窗口只用来截获首个 URL：外站交给系统浏览器，本机链接改回主
                // WebView。**任何情况下都销毁弹窗**——否则它会把内容加载进去并常驻
                // （真机上用 CDP 观测到过第二个 WebView target 泄漏）。
                override fun onCreateWindow(
                    view: WebView, isDialog: Boolean, isUserGesture: Boolean, resultMsg: android.os.Message
                ): Boolean {
                    val transport = resultMsg.obj as WebView.WebViewTransport
                    // 用 applicationContext：弹窗不被 Activity 持有，即使销毁路径被绕过
                    // 也不会把 Activity 泄漏出去。
                    val popup = WebView(applicationContext)
                    var destroyed = false
                    val safeDestroy = {
                        if (!destroyed) { destroyed = true; popup.destroy() }
                    }
                    popup.webViewClient = object : WebViewClient() {
                        override fun shouldOverrideUrlLoading(
                            v: WebView, request: WebResourceRequest
                        ): Boolean {
                            val uri = request.url
                            val gw = isGateway(uri)
                            safeDestroy()
                            if (gw) webView.loadUrl(uri.toString()) else handleExternal(uri)
                            return true
                        }
                    }
                    transport.webView = popup
                    resultMsg.sendToTarget()
                    // 兜底：首批导航若是 about:blank / javascript: / 同文档，回调不触发，
                    // 弹窗会常驻。给 3s 宽限，仍没被销毁就强制销毁。
                    ui.postDelayed({ safeDestroy() }, 3000)
                    return true
                }

                override fun onConsoleMessage(msg: ConsoleMessage): Boolean {
                    if (BuildConfig.DEBUG) Log.d("PanelWebView", "${msg.message()} @${msg.lineNumber()}")
                    return true
                }
            }
        }
        setContentView(webView)

        requestNotificationPermission()
        requestBatteryExemption()
        maybeGuideBackground()
        GatewayService.start(this)
        showBooting()
        waitThenLoadPanel()
    }

    /**
     * ColorOS 等国产 ROM 的额外保活引导。
     *
     * 标准电池优化白名单挡不住 ROM 自己的后台冻结（实测：白名单已加、前台服务在跑，
     * 退到后台 20s 后整 UID 的 cgroup 仍被冻结，网关静默）。只有「允许后台运行」
     * 开关有效，而它无法由 App 运行时自授，只能引导用户去点。只提示一次。
     */
    private fun maybeGuideBackground() {
        if (BackgroundGuide.alreadyAsked(this)) return
        if (!BackgroundGuide.isColorOS(this)) return
        BackgroundGuide.markAsked(this)
        AlertDialog.Builder(this)
            .setTitle("还需要一步：允许后台运行")
            .setMessage(BackgroundGuide.instruction(this))
            .setPositiveButton("去设置") { _, _ -> BackgroundGuide.open(this) }
            .setNegativeButton("以后再说", null)
            .show()
    }

    /**
     * 申请忽略电池优化。
     *
     * 这是国产 ROM（MIUI / JOYUI / 黑鲨）上最容易杀死后台服务的机制：息屏几分钟后
     * 系统就把进程冻结/杀掉，网关随之掉线。只申请一次，且只在系统允许时才弹（部分
     * ROM 会把它导向设置页）；用户拒绝也不影响使用，只是后台存活差一些。
     */
    private fun requestBatteryExemption() {
        if (Build.VERSION.SDK_INT < 23) return
        val pm = getSystemService(android.os.PowerManager::class.java) ?: return
        if (pm.isIgnoringBatteryOptimizations(packageName)) return
        runCatching {
            startActivity(
                Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
                    .setData(Uri.parse("package:$packageName"))
            )
        }.onFailure {
            runCatching { startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) }
        }
    }

    // ---------------------------------------------------- 外链与文件选择

    /**
     * 把非本机地址转发给系统浏览器。
     *
     * 只放行网关自己的地址（面板/API）在 WebView 内导航；华为云 OAuth 授权页等
     * 外站一律用 ACTION_VIEW 打开，避免在面板 WebView 里加载第三方页面。
     *
     * @return true 表示已自行处理（WebView 不再导航）。
     */
    private fun handleExternal(uri: Uri): Boolean {
        if (isGateway(uri)) return false   // 网关自己，留在 WebView 内
        // 其余一律交给系统浏览器：非 http(s)（tel:/mailto:/自定义 scheme）也走这里。
        return try {
            startActivity(Intent(Intent.ACTION_VIEW, uri).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            true
        } catch (e: ActivityNotFoundException) {
            Log.w(TAG, "no app for ${uri.scheme}:${uri.host}")
            true
        }
    }

    /**
     * 是否指向本进程的网关。
     *
     * 必须**同时**校验 host 与端口：只认 host 的话，任何来源的页面都能让 WebView
     * 去访问设备上其它监听的本地服务（http://127.0.0.1:8080 等）——那是本机端口扫描面。
     */
    private fun isGateway(uri: Uri): Boolean {
        val host = uri.host.orEmpty()
        if (host != "127.0.0.1" && host != "localhost") return false
        return uri.port == GatewayRunner.port
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode == FILE_CHOOSER_REQUEST) {
            val cb = filePathCallback
            filePathCallback = null
            cb?.onReceiveValue(
                if (resultCode == RESULT_OK && data != null) {
                    WebChromeClient.FileChooserParams.parseResult(resultCode, data)
                } else null
            )
            return
        }
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
    }

    // ------------------------------------------------------------- 加载

    /** 轮询 /healthz，就绪后再加载面板（网关冷启动需要一点时间）。 */
    private fun waitThenLoadPanel(attempt: Int = 0) {
        if (destroyed) return
        probe.execute {
            val ready = GatewayRunner.probeHealth()
            ui.post {
                if (destroyed) return@post
                when {
                    ready -> webView.loadUrl("${GatewayRunner.baseUrl()}/panel")
                    attempt < MAX_ATTEMPTS -> ui.postDelayed({ waitThenLoadPanel(attempt + 1) }, RETRY_MS)
                    else -> webView.loadUrl("${GatewayRunner.baseUrl()}/panel") // 显示真实错误
                }
            }
        }
    }

    private fun showBooting() {
        val html = """
            <!doctype html><html><head><meta name="viewport"
            content="width=device-width,initial-scale=1"><style>
            body{margin:0;height:100vh;display:flex;align-items:center;justify-content:center;
            font-family:system-ui,sans-serif;background:#0f1115;color:#c9d1d9}
            .b{text-align:center}.s{width:28px;height:28px;margin:0 auto 16px;border-radius:50%;
            border:3px solid #2a2f3a;border-top-color:#4c8dff;animation:r 1s linear infinite}
            @keyframes r{to{transform:rotate(360deg)}}</style></head>
            <body><div class="b"><div class="s"></div>网关启动中…</div></body></html>
        """.trimIndent()
        webView.loadDataWithBaseURL(null, html, "text/html", "utf-8", null)
    }

    /** 从后台返回时，若网关已死则拉起，并刷新面板。 */
    override fun onResume() {
        super.onResume()
        if (destroyed) return
        if (!GatewayRunner.isRunning()) GatewayService.start(this)
    }

    override fun onBackPressed() {
        // 面板内导航优先回退；否则回桌面，网关随后台服务继续跑（服务别人用）。
        if (injected && webView.canGoBack()) webView.goBack() else moveTaskToBack(true)
    }

    override fun onDestroy() {
        destroyed = true
        filePathCallback?.onReceiveValue(null)
        filePathCallback = null
        probe.shutdownNow()
        ui.removeCallbacksAndMessages(null)
        webView.destroy()
        super.onDestroy()
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
    }

    companion object {
        private const val TAG = "MainActivity"
        private const val RETRY_MS = 250L
        private const val MAX_ATTEMPTS = 120   // ~30s，冷启动足够宽松
        private const val FILE_CHOOSER_REQUEST = 1001
    }
}
