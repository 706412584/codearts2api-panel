package com.codearts.panel

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

/**
 * 常驻前台服务：把网关子进程护在后台。
 *
 * 为什么需要这些机制（沿用 loomy2api 的实践）：
 *  1. 单纯的前台服务只提升**进程**优先级，息屏后 CPU 仍可能挂起，子进程被冻结、
 *     在途请求全断。故全程持有 PARTIAL_WAKE_LOCK（onDestroy 释放，进程被杀由内核
 *     回收）。
 *  2. 看门狗每 30s 探一次 /healthz，连续失败即重启子进程（Go 进程重启是干净的，
 *     不像 Python 有解释器全局状态）。
 *  3. START_STICKY：被后台杀死后系统重启服务，onStartCommand 里重新拉起网关。
 *  4. onTaskRemoved：从最近任务划掉时不退出，网关是给别人用的服务端。
 */
class GatewayService : Service() {

    companion object {
        const val CHANNEL = "gateway"
        private const val NOTIFICATION_ID = 1
        private const val WAKE_LOCK_TAG = "codearts2api:gateway"
        private const val ACTION_STOP = "com.codearts.panel.action.STOP_GATEWAY"

        /** 看门狗探测间隔。 */
        private const val HEALTH_INTERVAL_MS = 30_000L

        /** 连续失败多少次才判定网关已死并重启。 */
        private const val FAILURES_BEFORE_RESTART = 2

        @Volatile
        var instance: GatewayService? = null
            private set

        /** 启动服务；重复调用安全。 */
        fun start(context: Context) {
            val intent = Intent(context, GatewayService::class.java)
            try {
                if (Build.VERSION.SDK_INT >= 26) context.startForegroundService(intent)
                else context.startService(intent)
            } catch (t: Throwable) {
                // 个别 ROM 拒绝后台 startForegroundService。
                android.util.Log.w("GatewayService", "start failed", t)
            }
        }

        fun stop(context: Context) {
            try {
                context.stopService(Intent(context, GatewayService::class.java))
            } catch (t: Throwable) {
                android.util.Log.w("GatewayService", "stop failed", t)
            }
        }
    }

    private var wakeLock: PowerManager.WakeLock? = null
    private val watchdog = Executors.newSingleThreadScheduledExecutor { r ->
        Thread(r, "gateway-watchdog").apply { isDaemon = true }
    }
    private val booting = AtomicBoolean(false)
    private var failures = 0
    @Volatile private var stopping = false

    override fun onCreate() {
        super.onCreate()
        GatewayRunner.loadPort(applicationContext)
        createChannel()

        // 先转前台：startForegroundService 必须 ~5s 内 startForeground，而配置种子
        // 生成 + 进程启动在冷启动时可能更慢。
        startForeground(NOTIFICATION_ID, buildNotification())
        instance = this

        acquireWakeLock()
        ensureGatewayRunning()
        startWatchdog()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopping = true
            GatewayRunner.stop()
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
            return START_NOT_STICKY
        }
        // START_STICKY 重启后进程已死，重新拉起网关。
        ensureGatewayRunning()
        return START_STICKY
    }

    /** 划掉任务不能带走网关——它是给别人用的服务端。重新拉起服务。 */
    override fun onTaskRemoved(rootIntent: Intent?) {
        if (!stopping) {
            try {
                if (Build.VERSION.SDK_INT >= 26) {
                    startForegroundService(Intent(this, GatewayService::class.java))
                } else {
                    startService(Intent(this, GatewayService::class.java))
                }
            } catch (t: Throwable) {
                android.util.Log.w("GatewayService", "restart after task removal failed", t)
            }
        }
        super.onTaskRemoved(rootIntent)
    }

    override fun onDestroy() {
        stopping = true
        try { watchdog.shutdownNow() } catch (_: Throwable) {}
        // 服务没了就不该留孤儿网关进程。用户显式「停止网关」已经停过，这里再兜一次。
        GatewayRunner.stop()
        releaseWakeLock()
        try { stopForeground(STOP_FOREGROUND_REMOVE) } catch (_: Throwable) {}
        instance = null
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    // ------------------------------------------------------------ gateway

    /** 拉起网关子进程（幂等）。 */
    private fun ensureGatewayRunning() {
        if (!booting.compareAndSet(false, true)) return
        Thread({
            try {
                if (GatewayRunner.ensureRunning(applicationContext)) {
                    android.util.Log.i("GatewayService", "gateway running on port ${GatewayRunner.port}")
                }
            } catch (t: Throwable) {
                android.util.Log.e("GatewayService", "gateway boot failed", t)
            } finally {
                booting.set(false)
            }
        }, "gateway-boot").start()
    }

    /** 探 /healthz；连续失败即重启子进程。 */
    private fun startWatchdog() {
        watchdog.scheduleWithFixedDelay({
            if (stopping) return@scheduleWithFixedDelay
            if (GatewayRunner.probeHealth()) {
                failures = 0
                return@scheduleWithFixedDelay
            }
            failures++
            if (failures < FAILURES_BEFORE_RESTART) return@scheduleWithFixedDelay
            failures = 0
            android.util.Log.w("GatewayService", "gateway unresponsive, restarting")
            try {
                GatewayRunner.restart(applicationContext)
            } catch (t: Throwable) {
                android.util.Log.e("GatewayService", "restart failed", t)
            }
        }, HEALTH_INTERVAL_MS, HEALTH_INTERVAL_MS, TimeUnit.MILLISECONDS)
    }

    // --------------------------------------------------------- wake lock

    private fun acquireWakeLock() {
        if (wakeLock != null) return
        val pm = getSystemService(Context.POWER_SERVICE) as? PowerManager ?: return
        try {
            // 无超时：由 onDestroy 释放。带超时会在请求中途静默过期，网关又被冻结。
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKE_LOCK_TAG)
                .also { it.setReferenceCounted(false); it.acquire() }
        } catch (t: Throwable) {
            android.util.Log.w("GatewayService", "acquire wakelock failed", t)
        }
    }

    private fun releaseWakeLock() {
        val lock = wakeLock ?: return
        wakeLock = null
        try {
            if (lock.isHeld) lock.release()
        } catch (_: Throwable) {
            // 内核会在进程死亡时回收
        }
    }

    // ------------------------------------------------------- notification

    private fun createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = getSystemService(NotificationManager::class.java)
        val channel = NotificationChannel(CHANNEL, "网关", NotificationManager.IMPORTANCE_LOW)
        channel.description = "本地 API 网关运行状态"
        channel.setShowBadge(false)
        nm.createNotificationChannel(channel)
    }

    private fun buildNotification(): Notification {
        val launch = packageManager.getLaunchIntentForPackage(packageName)
        val contentIntent = launch?.let {
            PendingIntent.getActivity(this, 0, it,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        }
        val stopIntent = PendingIntent.getService(
            this, 1,
            Intent(this, GatewayService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)

        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setContentTitle("网关运行中")
            .setContentText("端口 ${GatewayRunner.port} · 点击管理")
            .setContentIntent(contentIntent)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "停止网关", stopIntent)
            .build()
    }
}
