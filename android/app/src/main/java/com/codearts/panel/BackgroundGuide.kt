package com.codearts.panel

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import android.util.Log

/**
 * 国产 ROM 的后台保活引导。
 *
 * 网关跑在 App 进程内、后台有前台服务，但 ColorOS / MIUI 这类 ROM 还会进一步把
 * 后台进程**冻结**（整 UID 的 cgroup），此时前台服务与 wake lock 都挡不住——只有
 * 系统的「允许后台运行」开关有效，而它**按设计无法由 App 自己在运行时授予**
 * （否则任何恶意软件都能把自己设成永不冻结）。
 *
 * 所以这里只做引导：把用户送到系统的应用耗电管理页，并说明要选哪一项。
 * ColorOS 实测路径：设置 → 电池 → 应用耗电管理 → 本应用 → 「完全允许后台行为」。
 */
object BackgroundGuide {

    private const val TAG = "BackgroundGuide"
    private const val PREFS = "codearts_settings"
    private const val KEY_ASKED = "background_guide_asked"

    /** ColorOS 自带的应用耗电管理页（本机实测存在，是**列表**，无逐应用深链）。 */
    private const val OPPO_BATTERY_PKG = "com.oplus.battery"
    private const val OPPO_POWER_PAGE =
        "com.oplus.powermanager.fuelgaue.PowerUsageModelActivity"

    /** 是否为 ColorOS 系（OPPO / realme / 一加）且装了它的电源管理页。 */
    fun isColorOS(ctx: Context): Boolean {
        val mfr = Build.MANUFACTURER.lowercase()
        val looksOppo = mfr.contains("oppo") || mfr.contains("realme") ||
            mfr.contains("oneplus") || mfr.contains("oplus")
        return looksOppo && hasPackage(ctx, OPPO_BATTERY_PKG)
    }

    /** 是否已经问过用户（只引导一次，避免每次启动都弹）。 */
    fun alreadyAsked(ctx: Context): Boolean =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(KEY_ASKED, false)

    fun markAsked(ctx: Context) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit().putBoolean(KEY_ASKED, true).apply()
    }

    /**
     * 打开系统的应用耗电管理页，依次尝试：ColorOS 自带页 → 系统电池优化页 → 应用详情页。
     *
     * @return true 表示成功打开了某一个页面。
     */
    fun open(ctx: Context): Boolean {
        if (isColorOS(ctx) &&
            tryStart(ctx, Intent().setComponent(ComponentName(OPPO_BATTERY_PKG, OPPO_POWER_PAGE)))
        ) return true

        if (tryStart(ctx, Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))) return true

        return tryStart(
            ctx,
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                .setData(Uri.parse("package:${ctx.packageName}"))
        )
    }

    /** 提示语：ColorOS 与其它 ROM 的说法不同。 */
    fun instruction(ctx: Context): String = if (isColorOS(ctx)) {
        "在「应用耗电管理」里找到 CodeArts2API，点进去选择「完全允许后台行为」。\n\n" +
            "否则息屏几分钟后系统会冻结网关，其他设备就连不上 API 了。"
    } else {
        "在电池/后台管理里把 CodeArts2API 设为「不允许系统优化 / 允许后台运行」，否则系统会冻结网关。"
    }

    private fun tryStart(ctx: Context, intent: Intent): Boolean = try {
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        ctx.startActivity(intent)
        true
    } catch (t: Throwable) {
        Log.w(TAG, "打不开 ${intent.component ?: intent.action}: ${t.message}")
        false
    }

    private fun hasPackage(ctx: Context, pkg: String): Boolean = try {
        ctx.packageManager.getPackageInfo(pkg, 0); true
    } catch (_: Throwable) {
        false
    }
}
