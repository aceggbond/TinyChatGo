package com.aceggbond.tinychatgo;

import android.Manifest;
import android.app.*;
import android.content.*;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.Uri;
import android.os.*;
import android.provider.Settings;
import android.view.*;
import android.webkit.*;
import android.widget.*;
import java.util.Locale;

public class MainActivity extends Activity {
    private static final String CHANNEL = "tinychatgo_messages";
    private WebView web;
    private ValueCallback<Uri[]> fileCallback;

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        createChannel();
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED)
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 20);
        String url = getPreferences(MODE_PRIVATE).getString("server", "");
        if (url.isEmpty()) showServerDialog(); else open(url);
    }

    private void showServerDialog() {
        final EditText input = new EditText(this);
        input.setHint("http://NAS地址:18080"); input.setSingleLine(true);
        input.setPadding(32, 8, 32, 8);
        new AlertDialog.Builder(this).setTitle("连接 TinyChatGo 服务端")
            .setMessage("请输入 NAS 服务端 HTTP 或 HTTPS 地址")
            .setView(input).setCancelable(false)
            .setPositiveButton("连接", (d, w) -> {
                String url = input.getText().toString().trim();
                if (!url.matches("(?i)^https?://.*")) url = "http://" + url;
                getPreferences(MODE_PRIVATE).edit().putString("server", url).apply(); open(url);
            }).show();
    }

    private void open(String url) {
        web = new WebView(this); web.setBackgroundColor(Color.WHITE); setContentView(web);
        WebSettings s = web.getSettings(); s.setJavaScriptEnabled(true); s.setDomStorageEnabled(true);
        s.setAllowFileAccess(true); s.setMediaPlaybackRequiresUserGesture(false);
        s.setUserAgentString(s.getUserAgentString() + " TinyChatGo-Android/1.0.0");
        web.addJavascriptInterface(new Bridge(), "TinyChatGoAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override public void onPageFinished(WebView v, String u) {
                v.evaluateJavascript("window.lanchatNotify=function(t,b,r,a,m,p){TinyChatGoAndroid.notify(String(t),String(b),String(r));return Promise.resolve()};window.clientAndroid=true", null);
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams p) {
                if (fileCallback != null) fileCallback.onReceiveValue(null); fileCallback = cb;
                startActivityForResult(p.createIntent(), 30); return true;
            }
        });
        web.setDownloadListener((u, agent, disposition, mime, length) -> {
            DownloadManager.Request request = new DownloadManager.Request(Uri.parse(u));
            request.addRequestHeader("User-Agent", agent); request.setMimeType(mime);
            request.setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
            request.setDestinationInExternalPublicDir(android.os.Environment.DIRECTORY_DOWNLOADS, URLUtil.guessFileName(u, disposition, mime));
            ((DownloadManager)getSystemService(DOWNLOAD_SERVICE)).enqueue(request);
        });
        web.loadUrl(url);
    }

    public final class Bridge {
        @JavascriptInterface public void notify(String title, String body, String route) {
            if (hasWindowFocus()) return;
            Intent intent = new Intent(MainActivity.this, MainActivity.class).setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP | Intent.FLAG_ACTIVITY_CLEAR_TOP);
            PendingIntent pending = PendingIntent.getActivity(MainActivity.this, route.hashCode(), intent, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
            Notification n = new Notification.Builder(MainActivity.this, CHANNEL).setSmallIcon(android.R.drawable.sym_action_chat)
                .setContentTitle(title).setContentText(body).setStyle(new Notification.BigTextStyle().bigText(body)).setAutoCancel(true).setContentIntent(pending).build();
            ((NotificationManager)getSystemService(NOTIFICATION_SERVICE)).notify((title + route).hashCode(), n);
        }
        @JavascriptInterface public void changeServer() { runOnUiThread(MainActivity.this::showServerDialog); }
    }

    private void createChannel() {
        NotificationChannel c = new NotificationChannel(CHANNEL, "聊天消息", NotificationManager.IMPORTANCE_HIGH);
        c.setDescription("TinyChatGo 新消息提醒"); ((NotificationManager)getSystemService(NOTIFICATION_SERVICE)).createNotificationChannel(c);
    }
    @Override protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request == 30 && fileCallback != null) { fileCallback.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(result, data)); fileCallback = null; }
    }
    @Override public void onBackPressed() { if (web != null && web.canGoBack()) web.goBack(); else super.onBackPressed(); }
}
