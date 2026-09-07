package com.daboshi.droidpool;

import android.app.UiAutomation;
import android.os.HandlerThread;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityWindowInfo;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.io.PrintWriter;
import java.lang.reflect.Constructor;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.List;

/**
 * 常驻 UI dump agent。
 *
 * 起因：`uiautomator dump` 一次要 2.3 秒，而它吐出来的树只有 26 个节点 / 9 KB
 * （2026-09-07 在 3588-a-8 实测）。逐层剥开后确认耗时全在每次调用重新起 ART
 * 进程（574~929 ms）再加载框架 jar、连接 AccessibilityService 上，遍历本身可以忽略。
 * 一个 login_flow 要十几次 dump，这笔开销是 agent 驱动设备的主要瓶颈。
 *
 * 所以这里不去优化遍历，而是把进程和连接**留住**：一次 UiAutomation 连接，
 * 之后每次请求只走 adb 往返加一次树遍历。用法与 scrcpy-server 一致，
 * 由 adb push 上来再用 app_process 拉起。
 *
 * 协议刻意做成一行一个请求的纯文本，方便用 nc 或 adb shell 直接调，排查时不用带工具：
 *   DUMP\n  -> 一行 XML（内部换行已转义），或 ERR <原因>
 *   PING\n  -> PONG
 *   QUIT\n  -> 关连接
 */
public final class UiAgent {

    private static final int DEFAULT_PORT = 27400;

    private UiAgent() {}

    public static void main(String[] args) {
        int port = DEFAULT_PORT;
        for (String a : args) {
            if (a.startsWith("port=")) port = Integer.parseInt(a.substring(5));
        }
        try {
            UiAutomation ua = connect();
            serve(port, ua);
        } catch (Throwable t) {
            System.err.println("uiagent 启动失败: " + t);
            t.printStackTrace();
            System.exit(1);
        }
    }

    /**
     * 建立 UiAutomation 连接。
     *
     * 全程反射，一个隐藏类都不在编译期引用——这样只用公开的 android.jar 就能编出来，
     * 不必依赖 AOSP 源码树里的 framework classes.jar。代价是签名变化只能在运行期发现，
     * 所以下面把两种已知构造签名都试一遍，并在都失败时把原因带出来：
     * 硬编码一种签名的话，换一版 Android 就变成「静默连不上」，最难查。
     */
    private static UiAutomation connect() throws Exception {
        HandlerThread ht = new HandlerThread("uiagent");
        ht.start();
        Class<?> connCls = Class.forName("android.app.UiAutomationConnection");
        Class<?> iconnCls = Class.forName("android.app.IUiAutomationConnection");
        Object conn = connCls.getDeclaredConstructor().newInstance();

        UiAutomation ua = null;
        Exception first = null;
        try {
            java.lang.reflect.Constructor<UiAutomation> c =
                    UiAutomation.class.getDeclaredConstructor(android.os.Looper.class, iconnCls);
            c.setAccessible(true);
            ua = c.newInstance(ht.getLooper(), conn);
        } catch (NoSuchMethodException e) {
            first = e;
        }
        if (ua == null) {
            java.lang.reflect.Constructor<UiAutomation> c =
                    UiAutomation.class.getDeclaredConstructor(java.util.concurrent.Executor.class, iconnCls);
            c.setAccessible(true);
            Object exec = Class.forName("android.os.HandlerExecutor")
                    .getDeclaredConstructor(android.os.Handler.class)
                    .newInstance(new android.os.Handler(ht.getLooper()));
            try {
                ua = c.newInstance(exec, conn);
            } catch (Exception e) {
                if (first != null) e.addSuppressed(first);
                throw e;
            }
        }
        java.lang.reflect.Method m = UiAutomation.class.getDeclaredMethod("connect");
        m.setAccessible(true);
        m.invoke(ua);
        return ua;
    }

    private static void serve(int port, UiAutomation ua) throws Exception {
        // 绑在 loopback：外面靠 adb forward 进来，不在设备网络上开口子
        ServerSocket ss = new ServerSocket(port, 8, java.net.InetAddress.getByName("127.0.0.1"));
        System.out.println("uiagent listening on 127.0.0.1:" + port);
        System.out.flush();
        while (true) {
            Socket s = ss.accept();
            // 单线程按序处理：dump 本身是毫秒级，并发只会让 UiAutomation 竞争
            handle(s, ua);
        }
    }

    private static void handle(Socket s, UiAutomation ua) {
        try (Socket sock = s) {
            sock.setTcpNoDelay(true);
            BufferedReader in = new BufferedReader(
                    new InputStreamReader(sock.getInputStream(), StandardCharsets.UTF_8));
            OutputStream os = sock.getOutputStream();
            PrintWriter out = new PrintWriter(new java.io.OutputStreamWriter(os, StandardCharsets.UTF_8), true);
            String line;
            while ((line = in.readLine()) != null) {
                line = line.trim();
                if (line.equalsIgnoreCase("QUIT")) return;
                if (line.equalsIgnoreCase("PING")) { out.println("PONG"); continue; }
                if (line.equalsIgnoreCase("DUMP")) {
                    try {
                        out.println(dump(ua));
                    } catch (Throwable t) {
                        out.println("ERR " + t);
                    }
                    continue;
                }
                out.println("ERR 未知指令: " + line);
            }
        } catch (Throwable ignored) {
            // 客户端断开是常态，不值得记
        }
    }

    /** 把当前所有窗口的可访问性树序列化成一行 XML。 */
    private static String dump(UiAutomation ua) {
        StringBuilder sb = new StringBuilder(16 * 1024);
        sb.append("<?xml version='1.0' encoding='UTF-8'?><hierarchy>");
        List<AccessibilityWindowInfo> windows = ua.getWindows();
        if (windows == null || windows.isEmpty()) {
            AccessibilityNodeInfo root = ua.getRootInActiveWindow();
            if (root != null) node(sb, root);
        } else {
            for (AccessibilityWindowInfo w : windows) {
                AccessibilityNodeInfo root = w.getRoot();
                if (root != null) node(sb, root);
            }
        }
        sb.append("</hierarchy>");
        return sb.toString();
    }

    private static void node(StringBuilder sb, AccessibilityNodeInfo n) {
        android.graphics.Rect b = new android.graphics.Rect();
        n.getBoundsInScreen(b);
        sb.append("<node class=\"").append(esc(n.getClassName()))
          .append("\" package=\"").append(esc(n.getPackageName()))
          .append("\" text=\"").append(esc(n.getText()))
          .append("\" content-desc=\"").append(esc(n.getContentDescription()))
          .append("\" resource-id=\"").append(esc(n.getViewIdResourceName()))
          .append("\" clickable=\"").append(n.isClickable())
          .append("\" enabled=\"").append(n.isEnabled())
          .append("\" focused=\"").append(n.isFocused())
          .append("\" scrollable=\"").append(n.isScrollable())
          .append("\" selected=\"").append(n.isSelected())
          .append("\" bounds=\"[").append(b.left).append(',').append(b.top)
          .append("][").append(b.right).append(',').append(b.bottom).append("]\">");
        for (int i = 0; i < n.getChildCount(); i++) {
            AccessibilityNodeInfo c = n.getChild(i);
            if (c != null) node(sb, c);
        }
        sb.append("</node>");
    }

    /** XML 转义。换行一并转掉——协议是一行一条响应，树里的换行会撑破帧。 */
    private static String esc(CharSequence cs) {
        if (cs == null) return "";
        StringBuilder o = new StringBuilder(cs.length() + 16);
        for (int i = 0; i < cs.length(); i++) {
            char c = cs.charAt(i);
            switch (c) {
                case '&': o.append("&amp;"); break;
                case '<': o.append("&lt;"); break;
                case '>': o.append("&gt;"); break;
                case '"': o.append("&quot;"); break;
                case '\'': o.append("&apos;"); break;
                case '\n': o.append("&#10;"); break;
                case '\r': o.append("&#13;"); break;
                default: o.append(c);
            }
        }
        return o.toString();
    }
}
