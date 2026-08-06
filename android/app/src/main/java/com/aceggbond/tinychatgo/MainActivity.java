package com.aceggbond.tinychatgo;

import android.Manifest;
import android.app.*;
import android.content.*;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.Uri;
import android.net.http.SslError;
import android.os.*;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.util.Base64;
import android.webkit.*;
import android.widget.*;
import org.json.JSONObject;
import java.nio.charset.StandardCharsets;
import java.security.KeyStore;
import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

public class MainActivity extends Activity {
    private static final String CHANNEL = "tinychatgo_messages";
    private static final String DEFAULT_SERVER = "https://39.97.183.32:5630/";
    private static final String KEY_ALIAS = "TinyChatGoAccessPassword";
    private WebView web;
    private ValueCallback<Uri[]> fileCallback;
    private boolean askingPassword;

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        createChannel();
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED)
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 20);
        open();
    }

    private void open() {
        web = new WebView(this); web.setBackgroundColor(Color.WHITE); setContentView(web);
        WebSettings s = web.getSettings(); s.setJavaScriptEnabled(true); s.setDomStorageEnabled(true);
        s.setAllowFileAccess(true); s.setMediaPlaybackRequiresUserGesture(false);
        s.setUserAgentString(s.getUserAgentString() + " TinyChatGo-Android/1.0.0");
        web.addJavascriptInterface(new Bridge(), "TinyChatGoAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
                Uri failed = Uri.parse(error.getUrl());
                if ("39.97.183.32".equals(failed.getHost()) && failed.getPort() == 5630) handler.proceed(); else handler.cancel();
            }
            @Override public void onPageFinished(WebView view, String url) {
                installBridge();
                if ("/__auth/access".equals(Uri.parse(url).getPath())) unlockAccessPage();
            }
            @Override public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                if (request.isForMainFrame()) Toast.makeText(MainActivity.this, "页面加载失败：" + error.getDescription(), Toast.LENGTH_LONG).show();
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams p) {
                if (fileCallback != null) fileCallback.onReceiveValue(null); fileCallback = cb;
                startActivityForResult(p.createIntent(), 30); return true;
            }
        });
        web.setDownloadListener((url, agent, disposition, mime, length) -> {
            DownloadManager.Request request = new DownloadManager.Request(Uri.parse(url));
            request.addRequestHeader("User-Agent", agent); request.setMimeType(mime);
            request.setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
            request.setDestinationInExternalPublicDir(android.os.Environment.DIRECTORY_DOWNLOADS, URLUtil.guessFileName(url, disposition, mime));
            ((DownloadManager)getSystemService(DOWNLOAD_SERVICE)).enqueue(request);
        });
        web.loadUrl(DEFAULT_SERVER);
    }

    private void installBridge() {
        String script = "window.lanchatNotify=function(t,b,r,a,m,p){TinyChatGoAndroid.notify(String(t),String(b),String(r));return Promise.resolve()};window.clientAndroid=true;"
            + "(function(){"
            + "var grid=document.getElementById('portal-grid');if(!grid||!grid.dataset||grid.dataset.chat!=='1')return;"
            + "document.documentElement.classList.add('tinychatgo-android');"
            + "if(!grid.querySelector('.portal-nav')){var nav=document.createElement('nav');nav.className='portal-nav android-portal-nav';"
            + "nav.innerHTML='<button class=\"active\" data-android-mode=\"sessions\"><b>☵</b><span>会话</span></button><button data-android-mode=\"online\"><b>●</b><span>通讯录</span></button><button data-android-settings><b>⚙</b><span>设置</span></button>';"
            + "grid.insertBefore(nav,grid.firstChild);"
            + "nav.querySelectorAll('[data-android-mode]').forEach(function(button){button.onclick=function(){var mode=button.getAttribute('data-android-mode'),tab=document.querySelector('.contact-tab[data-mode=\"'+mode+'\"]');if(tab)tab.click();nav.querySelectorAll('[data-android-mode]').forEach(function(x){x.classList.toggle('active',x===button)})}});"
            + "nav.querySelector('[data-android-settings]').onclick=function(){var button=document.getElementById('portal-settings-button');if(button)button.click()};}"
            + "if(!document.getElementById('tinychatgo-android-style')){var style=document.createElement('style');style.id='tinychatgo-android-style';style.textContent='"
            + "html.tinychatgo-android,html.tinychatgo-android body{height:100%!important;min-height:0!important;overflow:hidden!important}"
            + ".tinychatgo-android .topbar{position:relative!important;height:64px!important;min-height:64px!important;padding:0 14px!important}"
            + ".tinychatgo-android .brand-sub{display:none!important}.tinychatgo-android .brand-logo{width:40px!important;height:40px!important}.tinychatgo-android .brand-name{font-size:18px!important}"
            + ".tinychatgo-android .portal-grid.layout-chat-users{display:grid!important;grid-template-columns:62px 210px minmax(0,1fr)!important;width:100%!important;height:calc(100vh - 64px)!important;height:calc(100dvh - 64px)!important;min-height:0!important;margin:0!important;border:0!important;border-radius:0!important;overflow:hidden!important}"
            + ".tinychatgo-android .portal-nav{display:flex!important;min-width:0!important;padding:10px 5px!important}.tinychatgo-android .portal-nav button{display:flex!important;flex-direction:column!important;align-items:center!important;justify-content:center!important;width:52px!important;min-height:48px!important;padding:4px 1px!important;font-size:9px!important}.tinychatgo-android .portal-nav button b{display:block;font-size:18px!important}"
            + ".tinychatgo-android .contacts{display:flex!important;min-width:0!important;height:100%!important;overflow:hidden!important}.tinychatgo-android .contacts-head{padding:10px 8px!important}.tinychatgo-android .contacts-title{font-size:14px!important}.tinychatgo-android .contact-tabs{display:none!important}.tinychatgo-android .contact-search{height:34px!important;margin-top:7px!important}.tinychatgo-android .contact-item{min-height:52px!important;padding:6px!important}"
            + ".tinychatgo-android .chat-panel{height:100%!important;min-height:0!important;overflow:hidden!important;grid-template-rows:auto auto minmax(0,1fr) auto!important}.tinychatgo-android .chat-head{min-height:62px!important;padding:8px 12px!important}.tinychatgo-android .chat-title{font-size:16px!important}.tinychatgo-android .messages{min-height:0!important;overflow-y:auto!important;padding:10px 12px!important}.tinychatgo-android .composer{position:relative!important;min-height:0!important;padding:7px 9px calc(7px + env(safe-area-inset-bottom))!important}.tinychatgo-android .composer textarea{min-height:52px!important;max-height:76px!important}.tinychatgo-android .bubble-row{max-width:92%!important}.tinychatgo-android .chat-image{max-height:260px!important}"
            + "@media(max-width:700px){.tinychatgo-android .portal-grid.layout-chat-users{grid-template-columns:52px 150px minmax(0,1fr)!important}.tinychatgo-android .portal-nav button{width:42px!important}.tinychatgo-android .contacts-title-row .contact-title-actions{display:none!important}.tinychatgo-android .contact-preview{display:none!important}.tinychatgo-android .avatar{width:32px!important;height:32px!important}.tinychatgo-android .contact-item{grid-template-columns:32px minmax(0,1fr)!important}.tinychatgo-android .chat-record-button{padding:0 6px!important;font-size:9px!important}}';document.head.appendChild(style);}"
            + "})();";
        web.evaluateJavascript(script, null);
    }

    private void unlockAccessPage() {
        String password = readPassword();
        if (password != null && !password.isEmpty()) { submitPassword(password); return; }
        if (askingPassword) return;
        askingPassword = true;
        EditText input = new EditText(this); input.setSingleLine(true); input.setInputType(0x81); input.setHint("访问密码");
        new AlertDialog.Builder(this).setTitle("TinyChatGo 访问验证").setMessage("首次使用请输入一次访问密码，密码将由 Android 系统加密保存。")
            .setView(input).setCancelable(false).setPositiveButton("继续", (dialog, which) -> {
                askingPassword = false; String value = input.getText().toString();
                if (!value.isEmpty()) { savePassword(value); submitPassword(value); }
            }).show();
    }

    private void submitPassword(String password) {
        String quoted = JSONObject.quote(password);
        String script = "fetch('/__auth/access',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:" + quoted + "})})"
            + ".then(function(r){if(!r.ok)throw new Error('访问密码错误');location.replace('/')})"
            + ".catch(function(e){document.getElementById('message').textContent=e.message})";
        web.evaluateJavascript(script, null);
    }

    private SecretKey key() throws Exception {
        KeyStore store = KeyStore.getInstance("AndroidKeyStore"); store.load(null);
        if (store.containsAlias(KEY_ALIAS)) return ((KeyStore.SecretKeyEntry)store.getEntry(KEY_ALIAS, null)).getSecretKey();
        KeyGenerator generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore");
        generator.init(new KeyGenParameterSpec.Builder(KEY_ALIAS, KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM).setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE).build());
        return generator.generateKey();
    }
    private void savePassword(String value) {
        try {
            Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding"); cipher.init(Cipher.ENCRYPT_MODE, key());
            String encrypted = Base64.encodeToString(cipher.doFinal(value.getBytes(StandardCharsets.UTF_8)), Base64.NO_WRAP);
            String iv = Base64.encodeToString(cipher.getIV(), Base64.NO_WRAP);
            getSharedPreferences("secure", MODE_PRIVATE).edit().putString("password", encrypted).putString("iv", iv).apply();
        } catch (Exception e) { Toast.makeText(this, "无法安全保存访问密码", Toast.LENGTH_LONG).show(); }
    }
    private String readPassword() {
        try {
            android.content.SharedPreferences p = getSharedPreferences("secure", MODE_PRIVATE);
            String encrypted = p.getString("password", ""), iv = p.getString("iv", ""); if (encrypted.isEmpty() || iv.isEmpty()) return null;
            Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
            cipher.init(Cipher.DECRYPT_MODE, key(), new GCMParameterSpec(128, Base64.decode(iv, Base64.NO_WRAP)));
            return new String(cipher.doFinal(Base64.decode(encrypted, Base64.NO_WRAP)), StandardCharsets.UTF_8);
        } catch (Exception e) { getSharedPreferences("secure", MODE_PRIVATE).edit().clear().apply(); return null; }
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
    }
    private void createChannel() {
        NotificationChannel channel = new NotificationChannel(CHANNEL, "聊天消息", NotificationManager.IMPORTANCE_HIGH);
        channel.setDescription("TinyChatGo 新消息提醒"); ((NotificationManager)getSystemService(NOTIFICATION_SERVICE)).createNotificationChannel(channel);
    }
    @Override protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request == 30 && fileCallback != null) { fileCallback.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(result, data)); fileCallback = null; }
    }
    @Override public void onBackPressed() { if (web != null && web.canGoBack()) web.goBack(); else super.onBackPressed(); }
}
