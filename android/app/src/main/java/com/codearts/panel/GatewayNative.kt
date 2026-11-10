package com.codearts.panel

/**
 * 网关 native 库（`libcodeartsgw.so`）的 JNI 绑定。
 *
 * 网关跑在 App 进程内（见 mobile/jni.go 的说明），因为 exec 出去的子进程会被 Android
 * 当作 phantom process 冻结——App 一退到后台监听就失效。
 */
object GatewayNative {

    @Volatile
    private var loaded = false

    /** 加载 native 库；重复调用安全。返回是否可用。 */
    fun ensureLoaded(): Boolean {
        if (loaded) return true
        synchronized(this) {
            if (loaded) return true
            loaded = try {
                System.loadLibrary("codeartsgw")
                true
            } catch (t: Throwable) {
                android.util.Log.e("GatewayNative", "loadLibrary failed", t)
                false
            }
        }
        return loaded
    }

    /**
     * 在后台 goroutine 上启动网关，绑定成功才返回。
     *
     * @param cfgPath config.json 的绝对路径
     * @return 0 表示已开始监听；非 0 表示启动失败（配置错误、端口被占等）
     */
    external fun start(cfgPath: String): Int

    /** 停止网关并等待退出；未运行时安全。 */
    external fun stop()
}
