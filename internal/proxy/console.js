// Injected by the tailyard proxy: forwards console output, uncaught errors
// and unhandled rejections to the terminal. Dev-only; never ship it.
(() => {
  if (window.__tailyard) return;
  window.__tailyard = true;

  const endpoint = "/__tailyard/console";
  const queue = [];
  let timer = 0;

  const flush = () => {
    timer = 0;
    if (!queue.length) return;
    const body = JSON.stringify(queue.splice(0));
    const small = body.length < 60000;
    if (small && navigator.sendBeacon) {
      navigator.sendBeacon(endpoint, new Blob([body], { type: "application/json" }));
      return;
    }
    fetch(endpoint, { method: "POST", body, keepalive: small, headers: { "Content-Type": "application/json" } }).catch(() => {});
  };

  const send = (level, text, stack) => {
    queue.push({ level, text: String(text).slice(0, 20000), stack: stack || "", url: location.pathname + location.search });
    if (!timer) timer = setTimeout(flush, 100);
  };

  const format = (value) => {
    if (typeof value === "string") return value;
    if (value instanceof Error) return value.stack || String(value);
    try {
      const seen = new WeakSet();
      return JSON.stringify(value, (_, v) => {
        if (typeof v === "bigint") return v.toString();
        if (typeof v === "object" && v !== null) {
          if (seen.has(v)) return "[Circular]";
          seen.add(v);
        }
        return v;
      }) ?? String(value);
    } catch {
      return String(value);
    }
  };

  for (const method of ["log", "info", "warn", "error", "debug"]) {
    const original = console[method];
    console[method] = function (...args) {
      try {
        const err = args.find((a) => a instanceof Error);
        send(method === "log" ? "info" : method, args.map(format).join(" "), err && err.stack);
      } catch {}
      return original.apply(this, args);
    };
  }

  addEventListener("error", (e) => send("error", e.message || format(e.error), e.error && e.error.stack));
  addEventListener("unhandledrejection", (e) => send("error", "Unhandled rejection: " + format(e.reason), e.reason && e.reason.stack));
  addEventListener("pagehide", flush);
})();
