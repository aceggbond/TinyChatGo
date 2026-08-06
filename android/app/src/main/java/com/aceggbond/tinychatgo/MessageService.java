package com.aceggbond.tinychatgo;

import android.app.*;
import android.content.*;
import android.net.Uri;
import android.net.http.SslError;
import android.os.*;
import android.webkit.*;

public class MessageService extends Service {
    private static final String SERVER = "https://39.97.183.32:5630/";
    private static final String SERVICE_CHANNEL = "tinychatgo_connection";
    private static final String MESSAGE_CHANNEL = "tinychatgo_messages";
    private final Handler handler = new Handler(Looper.getMainLooper());
    private WebView web;
    private PowerManager.WakeLock wakeLock;
    private static volatile MessageService instance;

    public static void setUserVisible(boolean visible) {
        MessageService service = instance;
        if (service == null) return;
        service.handler.post(visible ? service::suspendConnection : service::resumeConnection);
    }

    @Override public void onCreate() {
        super.onCreate(); instance = this; createChannels(); startForeground(7, serviceNotification());
        wakeLock = ((PowerManager)getSystemService(POWER_SERVICE)).newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "TinyChatGo:Messages");
        wakeLock.acquire(); createWebView();
    }

    private void createWebView() {
        web = new WebView(getApplicationContext());
        web.setRendererPriorityPolicy(WebView.RENDERER_PRIORITY_IMPORTANT, true);
        WebSettings settings = web.getSettings(); settings.setJavaScriptEnabled(true); settings.setDomStorageEnabled(true);
        settings.setUserAgentString(settings.getUserAgentString() + " TinyChatGo-Android-Background/1.0.0");
        web.addJavascriptInterface(new BackgroundBridge(), "TinyChatGoBackground");
        web.setWebViewClient(new WebViewClient() {
            @Override public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
                Uri failed = Uri.parse(error.getUrl());
                if ("39.97.183.32".equals(failed.getHost()) && failed.getPort() == 5630) handler.proceed(); else handler.cancel();
            }
            @Override public void onPageFinished(WebView view, String url) {
                String script = "window.lanchatNotify=function(t,b,r,a,m,p){TinyChatGoBackground.notify(String(t),String(b),String(r));return Promise.resolve()};"
                    + "window.lanchatUnread=function(n,t){TinyChatGoBackground.unread(Number(n)||0);return Promise.resolve()};window.lanchatNativeWindowVisible=false";
                view.evaluateJavascript(script, null);
                if (url.startsWith(SERVER)) { handler.removeCallbacks(healthCheck); handler.postDelayed(healthCheck, 45000); }
            }
        });
        web.loadUrl(MainActivity.isVisible() ? "about:blank" : SERVER);
    }

    private void suspendConnection() { handler.removeCallbacks(healthCheck); if (web != null) web.loadUrl("about:blank"); }
    private void resumeConnection() { if (web != null && (web.getUrl() == null || !web.getUrl().startsWith(SERVER))) web.loadUrl(SERVER); }

    private final Runnable healthCheck = new Runnable() {
        @Override public void run() {
            if (web == null) return;
            web.evaluateJavascript("(document.getElementById('top-connection')&&!document.getElementById('top-connection').classList.contains('offline'))?'ok':'retry'", value -> {
                if (!"\"ok\"".equals(value)) web.reload();
            });
            handler.postDelayed(this, 45000);
        }
    };

    public final class BackgroundBridge {
        @JavascriptInterface public void notify(String title, String body, String route) {
            if (MainActivity.isVisible()) return;
            Intent open = new Intent(MessageService.this, MainActivity.class).putExtra("route", route)
                .setFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_SINGLE_TOP | Intent.FLAG_ACTIVITY_CLEAR_TOP);
            PendingIntent pending = PendingIntent.getActivity(MessageService.this, route.hashCode(), open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
            Notification notification = new Notification.Builder(MessageService.this, MESSAGE_CHANNEL)
                .setSmallIcon(android.R.drawable.sym_action_chat).setContentTitle(title).setContentText(body)
                .setStyle(new Notification.BigTextStyle().bigText(body)).setAutoCancel(true).setContentIntent(pending).build();
            ((NotificationManager)getSystemService(NOTIFICATION_SERVICE)).notify((title + route).hashCode(), notification);
        }
        @JavascriptInterface public void unread(int count) {
            getSharedPreferences("state", MODE_PRIVATE).edit().putInt("unread", count).apply();
        }
    }

    private void createChannels() {
        NotificationManager manager = (NotificationManager)getSystemService(NOTIFICATION_SERVICE);
        NotificationChannel service = new NotificationChannel(SERVICE_CHANNEL, "后台消息连接", NotificationManager.IMPORTANCE_LOW);
        service.setDescription("保持 TinyChatGo 后台消息连接"); service.setShowBadge(false); manager.createNotificationChannel(service);
        NotificationChannel messages = new NotificationChannel(MESSAGE_CHANNEL, "聊天消息", NotificationManager.IMPORTANCE_HIGH);
        messages.setDescription("TinyChatGo 新消息提醒"); manager.createNotificationChannel(messages);
    }
    private Notification serviceNotification() {
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent pending = PendingIntent.getActivity(this, 7, open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        return new Notification.Builder(this, SERVICE_CHANNEL).setSmallIcon(android.R.drawable.stat_notify_sync_noanim)
            .setContentTitle("TinyChatGo 消息服务").setContentText("后台消息连接运行中").setOngoing(true).setContentIntent(pending).build();
    }
    @Override public int onStartCommand(Intent intent, int flags, int startId) { return START_STICKY; }
    @Override public android.os.IBinder onBind(Intent intent) { return null; }
    @Override public void onDestroy() {
        instance = null; handler.removeCallbacksAndMessages(null); if (web != null) { web.destroy(); web = null; }
        if (wakeLock != null && wakeLock.isHeld()) wakeLock.release(); super.onDestroy();
    }
}
