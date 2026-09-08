import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.PrintWriter;
import java.net.Inet4Address;
import java.net.Inet6Address;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.TimeUnit;

/**
 * Ticket 01 loopback spike probe. Source-launched: {@code java Probe.java <args>}.
 *
 * <p>Modes (first arg):
 * <ul>
 *   <li>{@code server <guardedPort> <exactPort>} — parent: binds a listener on
 *       127.0.0.1:__EXACT_PORT__ and on a dynamic port, spawns a child JVM, then
 *       probes the guarded port over v4 and v6 plus external egress.</li>
 *   <li>{@code child <parentPort>} — child: spawns a grandchild, and both connect
 *       back to the parent's listener (dynamic-port loopback proof, one level
 *       removed from the profiled process).</li>
 *   <li>{@code grandchild <parentPort>} — grandchild: connects back to the
 *       parent's listener (two levels removed).</li>
 * </ul>
 *
 * <p>Every probe prints exactly one line:
 * {@code RESULT <probe> <family> OK|FAIL <detail>}
 * where {@code <probe>} is one of {@code child-loopback, grandchild-loopback,
 * exact-port-loopback, dynamic-loopback, guarded-v4, guarded-v6, external-egress}
 * and {@code <family>} is {@code ipv4}, {@code ipv6}, or {@code ext}.
 *
 * <p>Runner must unset JDK_JAVA_OPTIONS / JAVA_TOOL_OPTIONS / *_PROXY env before
 * launching, otherwise JVM-injected flags (a) skew IPv4-vs-IPv6 behavior
 * (preferIPv4Stack) and (b) route java.net.HttpURLConnection through a proxy.
 */
public final class Probe {

    private static final int CONNECT_TIMEOUT_MS = 2_000;
    private static final int EXT_CONNECT_TIMEOUT_MS = 3_000;
    private static final String EXTERNAL_TARGET = "1.1.1.1";
    private static final int EXTERNAL_PORT = 443;

    private Probe() {}

    public static void main(String[] args) throws Exception {
        if (args.length < 1) {
            System.err.println("usage: Probe <server <guardedPort> <exactPort> | child <parentPort> | grandchild <parentPort>>");
            System.exit(2);
        }
        switch (args[0]) {
            case "server" -> runServer(Integer.parseInt(args[1]), Integer.parseInt(args[2]));
            case "child" -> runChild(Integer.parseInt(args[1]));
            case "grandchild" -> runGrandchild(Integer.parseInt(args[1]));
            default -> {
                System.err.println("unknown mode: " + args[0]);
                System.exit(2);
            }
        }
    }

    // ---------------------------------------------------------------- server

    /** Parent: two listeners (one on the predeclared exact port, one dynamic),
     * then the child JVM, then the guarded/external probes. */
    private static void runServer(int guardedPort, int exactPort) throws Exception {
        // Bind the predeclared "exact" port (used by the exact-port posture).
        try (ServerSocket exact = new ServerSocket()) {
            exact.bind(new InetSocketAddress(loopback4(), exactPort));

            // Bind a dynamic port (mirrors Gradle daemon / Worker API port selection).
            try (ServerSocket dynamic = new ServerSocket()) {
                dynamic.bind(new InetSocketAddress(loopback4(), 0));
                int dynamicPort = dynamic.getLocalPort();
                System.out.println("PARENT-DYNAMIC-PORT " + dynamicPort);
                System.out.println("PARENT-EXACT-PORT " + exactPort);
                System.out.flush();

                // One-shot acceptor threads so connect probes complete TCP handshake
                // even when the posture allows them, rather than only reaching SYN.
                Thread exactAcc = acceptOnce(exact, "exact");
                Thread dynAcc = acceptOnce(dynamic, "dynamic");

                // Child JVM (same java binary, connect-mode, no inherited sandbox
                // change — Seatbelt propagates across exec).
                int childRc = launchChild("child", dynamicPort);
                result("child-loopback", "ipv4", childRc == 0,
                        childRc == 0 ? "child connected (rc=0)" : "child rc=" + childRc);

                // Self-connect probes from the parent (direct, no JVM hop).
                probe("exact-port-loopback", "ipv4", loopback4(), exactPort);
                probe("dynamic-loopback", "ipv4", loopback4(), dynamicPort);

                // Guarded host listener probes (v4 then v6).
                probe("guarded-v4", "ipv4", loopback4(), guardedPort);
                InetAddress lo6 = InetAddress.getByName("::1");
                probe("guarded-v6", "ipv6", lo6, guardedPort);

                // External egress control probe.
                probe("external-egress", "ext",
                        InetAddress.getByName(EXTERNAL_TARGET), EXTERNAL_PORT);

                exactAcc.join(TimeUnit.SECONDS.toMillis(5));
                dynAcc.join(TimeUnit.SECONDS.toMillis(5));
            }
        }
        // neutral trailing newline so log parsers see a clean EOF marker
        System.out.println("PROBE-DONE");
        System.out.flush();
    }

    /** Child: connect back to parent listener, then spawn grandchild to do the same. */
    private static void runChild(int parentPort) throws IOException, InterruptedException {
        boolean self = tryConnect(loopback4(), parentPort);
        long t0 = System.nanoTime();
        int rc = launchChild("grandchild", parentPort);
        long ms = (System.nanoTime() - t0) / 1_000_000L;
        if (!self) {
            System.err.println("child self-connect to parent port failed");
            System.exit(3);
        }
        result("grandchild-loopback", "ipv4", rc == 0,
                rc == 0 ? "grandchild connected (rc=0) in " + ms + "ms" : "grandchild rc=" + rc);
        System.exit(rc == 0 ? 0 : 4);
    }

    /** Grandchild: connect back to parent listener two levels down. */
    private static void runGrandchild(int parentPort) throws IOException {
        boolean ok = tryConnect(loopback4(), parentPort);
        System.exit(ok ? 0 : 5);
    }

    // ---------------------------------------------------------------- probes

    private static void probe(String name, String family, InetAddress addr, int port) {
        long t0 = System.nanoTime();
        try (Socket s = new Socket()) {
            s.connect(new InetSocketAddress(addr, port),
                    "ext".equals(family) ? EXT_CONNECT_TIMEOUT_MS : CONNECT_TIMEOUT_MS);
            long ms = (System.nanoTime() - t0) / 1_000_000L;
            result(name, family, true,
                    "connected to " + addr.getHostAddress() + ":" + port
                            + " local=" + s.getLocalAddress().getHostAddress()
                            + " in " + ms + "ms");
        } catch (Throwable t) {
            result(name, family, false, describe(t));
        }
    }

    private static boolean tryConnect(InetAddress addr, int port) {
        try (Socket s = new Socket()) {
            s.connect(new InetSocketAddress(addr, port), CONNECT_TIMEOUT_MS);
            return true;
        } catch (Throwable t) {
            return false;
        }
    }

    private static void result(String name, String family, boolean ok, String detail) {
        System.out.println("RESULT " + name + " " + family + " "
                + (ok ? "OK" : "FAIL") + " " + detail);
        System.out.flush();
    }

    // ---------------------------------------------------------------- helpers

    /** One-shot acceptor that swallows a single connection and reports it on stderr
     * (visible in the posture log, proving the handshake really happened). */
    private static Thread acceptOnce(ServerSocket ss, String label) {
        Thread t = new Thread(() -> {
            try {
                ss.setSoTimeout((int) TimeUnit.SECONDS.toMillis(8));
                try (Socket c = ss.accept()) {
                    System.err.println("ACCEPT[" + label + "] from "
                            + c.getRemoteSocketAddress());
                }
            } catch (Throwable ignored) {
                // timeout or posture-denied; parent probes already recorded the outcome
            }
        }, "accept-" + label);
        t.setDaemon(true);
        t.start();
        return t;
    }

    /** Relaunch this same class file in a descendant JVM using the running
     * java binary. InheritIO so descendant RESULT/ACCEPT lines land in the
     * posture log. Env scrubbed of JDK/proxy injection by the runner already;
     * we additionally strip anything JVM-specific we ourselves added. */
    private static int launchChild(String mode, int port) throws IOException, InterruptedException {
        String javaBin = Path.of(System.getProperty("java.home"), "bin", "java").toString();
        String self = sourceFile();
        List<String> cmd = new ArrayList<>(List.of(javaBin, self, mode, Integer.toString(port)));
        ProcessBuilder pb = new ProcessBuilder(cmd);
        pb.inheritIO();
        // Strip any JVM flag injection that would force an address-family bias
        // in descendants even if the parent env was polluted.
        pb.environment().remove("JDK_JAVA_OPTIONS");
        pb.environment().remove("JAVA_TOOL_OPTIONS");
        pb.environment().remove("_JAVA_OPTIONS");
        Process p = pb.start();
        boolean finished = p.waitFor(20, TimeUnit.SECONDS);
        if (!finished) {
            p.destroyForcibly();
            return 97;
        }
        return p.exitValue();
    }

    /** Resolve this source file. There is no standard system property exposing
     * the source path of a source-launched program, so the runner exports
     * PROBE_SOURCE explicitly; candidates below tolerate being run by hand from
     * the fixture dir as well. */
    private static String sourceFile() throws IOException {
        String fromEnv = System.getenv("PROBE_SOURCE");
        if (fromEnv != null && !fromEnv.isBlank() && Path.of(fromEnv).toFile().isFile()) {
            return fromEnv;
        }
        String fromProp = System.getProperty("sun.java.launcher.sourcepath"); // nonstandard
        if (fromProp != null && !fromProp.isBlank() && Path.of(fromProp).toFile().isFile()) {
            return fromProp;
        }
        for (String cand : new String[]{"Probe.java", "testdata/loopback-spike/Probe.java",
                "internal/sandboxrun/testdata/loopback-spike/Probe.java"}) {
            Path p = Path.of(cand).toAbsolutePath();
            if (p.toFile().isFile()) return p.toString();
        }
        throw new IOException("cannot locate Probe.java source for child re-launch; cwd="
                + Path.of("").toAbsolutePath() + " (set PROBE_SOURCE)");
    }

    /** Explicit 127.0.0.1 (never ::1) so dynamic-loopback and child/grandchild
     * probes always exercise the v4 path regardless of java.net.preferIPv4Stack,
     * which the scrubbed env leaves unset. The v6 dimension is probed explicitly
     * via guarded-v6. */
    private static InetAddress loopback4() {
        try {
            InetAddress a = InetAddress.getByName("127.0.0.1");
            if (a instanceof Inet4Address) return a;
        } catch (Throwable ignored) {
        }
        return InetAddress.getLoopbackAddress();
    }

    private static String describe(Throwable t) {
        String msg = t.getMessage();
        String cls = t.getClass().getSimpleName();
        // Normalize common JVM network failure text so log grepping is stable.
        String norm = (msg == null ? "" : msg)
                .replaceAll("\\s+", " ")
                .trim();
        String family = familyHint(t);
        return cls + "(" + norm + ")" + (family.isEmpty() ? "" : " " + family);
    }

    private static String familyHint(Throwable t) {
        // SocketException on macOS carries the errno text; record it verbatim —
        // the report needs the native error, not just "Connection refused".
        String m = t.getMessage();
        if (m == null) return "";
        String l = m.toLowerCase(Locale.ROOT);
        if (l.contains("operation not permitted")) return "errno=EPERM";
        if (l.contains("connection refused")) return "errno=ECONNREFUSED";
        if (l.contains("timed out") || l.contains("timeout")) return "errno=ETIMEDOUT";
        if (l.contains("no route")) return "errno=EHOSTUNREACH";
        return "";
    }

    static {
        // Silence the "Picked up JAVA_TOOL_OPTIONS" banner is impossible from
        // inside the JVM; the runner strips the env so the banner won't print.
    }
}
