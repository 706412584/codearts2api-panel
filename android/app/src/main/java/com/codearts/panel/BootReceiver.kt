package com.codearts.panel

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * 开机/升级后自动拉起网关服务，让手机重启后 API 即刻可用。
 *
 * 监听 BOOT_COMPLETED、MY_PACKAGE_REPLACED 以及部分 ROM 的 QUICKBOOT_POWERON。
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED,
            "android.intent.action.QUICKBOOT_POWERON",
            "com.htc.intent.action.QUICKBOOT_POWERON",
            -> GatewayService.start(context)
        }
    }
}
