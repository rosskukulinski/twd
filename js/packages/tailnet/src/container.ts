// ctx.container on twd (ADR-032 D1): a polyfill of Cloudflare's
// Durable Object container API, installed by the platform wrapper on every
// class that has a wrangler "containers" entry. It drives the object's own
// container (one Firecracker microVM, owned by twd-hostd) through the
// platform's TailnetApi, which attests the calling object: an object can
// only ever reach its own container.
//
// Semantics follow developers.cloudflare.com/durable-objects/api/container
// (start() returns at once and later calls wait for it; exec() has no shell,
// no timeout, and inherits only PATH; monitor() resolves on exit 0 and
// rejects otherwise), so @cloudflare/containers, Sandbox SDK 0.x and the
// Sandbox SDK 1.0 Files helper run unmodified.

export interface ContainerSpec {
  image?: string; // the class's default image ref
  images?: Record<string, string>; // named images (ctx.container.images)
}

interface Tw {
  ctr(op: string, args: any): Promise<any>;
  ctrExec(args: any): Promise<Response>;
  ctrStdin(args: any, body: ReadableStream): Promise<any>;
  ctrFetch(args: any, req: Request): Promise<Response>;
}

// Cloudflare's error strings, matched by @cloudflare/containers.
const NOT_LISTENING = "the container is not listening";
const NOT_RUNNING = "the container is not running, consider calling start()";
const UNEXPECTED_EXIT = "Container exited with unexpected exit code: ";
const RUNTIME_SIGNALLED = "Runtime signalled the container to exit: ";

const STATE_KEY = "__twd_container";

function err(body: any, fallback: string): Error {
  const code = body?.code ?? "container_error";
  let msg = String(body?.message ?? body?.detail ?? fallback);
  if (code === "not_listening") msg = NOT_LISTENING + " (" + msg + ")";
  else if (code === "container_not_running") msg = NOT_RUNNING;
  const e: any = new Error(msg);
  e.code = code;
  return e;
}

async function unwrap(p: Promise<any>): Promise<any> {
  const r = await p;
  if (!r || !r.ok) throw err(r?.error, "container op failed");
  return r.value;
}

async function errorOf(r: Response): Promise<Error> {
  let body: any = { code: r.headers.get("twd-error") ?? "container_error", message: "HTTP " + r.status };
  try {
    body = await r.json();
  } catch {
    /* not JSON */
  }
  return err(body, "HTTP " + r.status);
}

function toBytes(chunks: Uint8Array[]): ArrayBuffer {
  let n = 0;
  for (const c of chunks) n += c.byteLength;
  const out = new Uint8Array(n);
  let o = 0;
  for (const c of chunks) {
    out.set(c, o);
    o += c.byteLength;
  }
  return out.buffer;
}

async function readAll(s: ReadableStream<Uint8Array> | null): Promise<ArrayBuffer> {
  if (!s) return new ArrayBuffer(0);
  const chunks: Uint8Array[] = [];
  const rd = s.getReader();
  for (;;) {
    const { done, value } = await rd.read();
    if (done) break;
    chunks.push(value);
  }
  return toBytes(chunks);
}

// An exec's stdio arrives as frames: kind (1 stdout, 2 stderr, 3 exit),
// u32 big-endian length, payload.
function demux(body: ReadableStream<Uint8Array>, wantOut: boolean, wantErr: boolean) {
  type Ctl = ReadableStreamDefaultController<Uint8Array>;
  const ctl: { out: Ctl | null; err: Ctl | null } = { out: null, err: null };
  const stdout = wantOut ? new ReadableStream<Uint8Array>({ start: (c) => void (ctl.out = c) }) : null;
  const stderr = wantErr ? new ReadableStream<Uint8Array>({ start: (c) => void (ctl.err = c) }) : null;
  const outC = () => ctl.out;
  const errC = () => ctl.err;
  const exitCode = (async () => {
    const rd = body.getReader();
    let buf = new Uint8Array(0);
    const fail = (e: unknown) => {
      for (const c of [outC(), errC()]) {
        try {
          c?.error(e);
        } catch {
          /* closed */
        }
      }
    };
    try {
      for (;;) {
        while (buf.byteLength >= 5) {
          const len = new DataView(buf.buffer, buf.byteOffset + 1, 4).getUint32(0);
          if (buf.byteLength < 5 + len) break;
          const kind = buf[0];
          const p = buf.slice(5, 5 + len);
          buf = buf.subarray(5 + len);
          if (kind === 1) outC()?.enqueue(p);
          else if (kind === 2) errC()?.enqueue(p);
          else if (kind === 3) {
            const code = p.byteLength === 4 ? new DataView(p.buffer, p.byteOffset, 4).getInt32(0) : -1;
            for (const c of [outC(), errC()]) {
              try {
                c?.close();
              } catch {
                /* cancelled */
              }
            }
            rd.cancel().catch(() => {});
            return code;
          }
        }
        const { done, value } = await rd.read();
        if (done) throw new Error("exec stream ended without an exit status");
        const nb = new Uint8Array(buf.byteLength + value.byteLength);
        nb.set(buf);
        nb.set(value, buf.byteLength);
        buf = nb;
      }
    } catch (e) {
      fail(e);
      throw e;
    }
  })();
  exitCode.catch(() => {});
  return { stdout, stderr, exitCode };
}

export function createContainer(tw: Tw, ctx: any, cls: string, spec: ContainerSpec, self?: { class: string; name: string }) {
  const as = self ? { as: self } : {};
  // Any answer saying there is no container corrects the sync view.
  const op = (name: string, args: any = {}) =>
    unwrap(tw.ctr(name, { ...as, ...args })).catch((e) => {
      if (e?.code === "container_not_running" && !starting) {
        running = false;
        save();
      }
      throw e;
    });
  const kv = ctx?.storage?.kv;
  const load = (): { running: boolean; gen: number } => {
    try {
      const v = kv?.get(STATE_KEY);
      if (v && typeof v === "object") return { running: !!v.running, gen: Number(v.gen) || 0 };
    } catch {
      /* not SQLite-backed */
    }
    return { running: false, gen: 0 };
  };
  const st = load();
  let running = st.running; // last known (sync `running`); reconciled below
  let gen = st.gen;
  let starting: Promise<void> | null = null;
  let startErr: unknown = null;
  const intercepts = new Map<string, unknown>();
  const save = () => {
    try {
      kv?.put(STATE_KEY, { running, gen });
    } catch {
      /* ignore */
    }
  };
  const keep = (p: Promise<unknown>) => {
    try {
      ctx.waitUntil?.(p);
    } catch {
      /* outside a request */
    }
  };
  // A background watch keeps the sync `running` true to hostd: it flips
  // when the container exits or hostd stops it (inactivity, max lifetime,
  // kill). It dies with the request it started in; every call re-arms it.
  let watching = 0;
  const watch = () => {
    const g = gen;
    if (!running || !g || watching === g) return;
    watching = g;
    // waitUntil keeps it past the response (workerd caps that; the next
    // call re-arms it).
    keep((async () => {
      try {
        for (;;) {
          const v = await op("monitor", { gen: g });
          if (v.done) {
            if (gen === g) {
              running = false;
              save();
            }
            return;
          }
        }
      } catch {
        /* cancelled with its request */
      } finally {
        if (watching === g) watching = 0;
      }
    })());
  };
  const settle = async () => {
    if (starting) await starting.catch(() => {});
    watch();
  };
  // The container may have stopped while this object was evicted, or been
  // started by a previous version of it (blue/green): ask hostd before any
  // event is delivered.
  const reconcile = async () => {
    try {
      const v = await op("status");
      running = !!v.running;
      gen = Number(v.gen) || 0;
      save();
      watch();
    } catch {
      /* keep the stored view */
    }
  };
  try {
    ctx.blockConcurrencyWhile?.(reconcile);
  } catch {
    /* ignore */
  }

  const c = {
    get running(): boolean {
      return running;
    },
    get images(): Record<string, string> {
      return { ...(spec.images ?? {}) };
    },
    start(options: any = {}): void {
      if (running) throw new Error("start() cannot be called on a container that is already running.");
      if (options?.containerSnapshot) throw Object.assign(new Error("containerSnapshot is not supported on twd yet (ADR-032 D6: v1.1)"), { code: "not_supported" });
      if (options?.directorySnapshots?.length) throw Object.assign(new Error("directorySnapshots are not supported on twd"), { code: "not_supported" });
      running = true;
      startErr = null;
      save();
      const req = {
        image: options.image,
        entrypoint: options.entrypoint,
        env: options.env,
        enableInternet: !!options.enableInternet,
        instance: options.instance,
        labels: options.labels,
      };
      const p: Promise<void> = op("start", { start: req }).then(
        (v) => {
          gen = Number(v.gen) || gen + 1;
          save();
          watch();
        },
        (e) => {
          running = false;
          startErr = e;
          save();
          throw e;
        },
      );
      starting = p;
      p.catch(() => {}).finally(() => {
        if (starting === p) starting = null;
      });
      keep(p.catch(() => {}));
    },
    async monitor(): Promise<void> {
      if (starting) await starting; // a failed start rejects monitor()
      else if (!running) {
        if (startErr) throw startErr;
        throw new Error(NOT_RUNNING);
      }
      const g = gen;
      for (;;) {
        const v = await op("monitor", { gen: g });
        if (!v.done) continue;
        if (gen === g) {
          running = false;
          save();
        }
        const code = Number(v.exitCode);
        const reason = String(v.reason ?? "");
        if (code === 0 || reason.startsWith("destroy")) return;
        if (reason === "exited") throw Object.assign(new Error(UNEXPECTED_EXIT + code), { exitCode: code });
        throw Object.assign(new Error(RUNTIME_SIGNALLED + (code >= 0 ? code : 137) + " (" + reason + ")"), { exitCode: code });
      }
    },
    async destroy(reason?: unknown): Promise<void> {
      await settle();
      await op("destroy", { reason: reason === undefined ? "" : String((reason as any)?.message ?? reason) });
      running = false;
      save();
    },
    signal(signo: number): void {
      if (!Number.isInteger(signo) || signo < 1 || signo > 64) throw new RangeError("signal: bad signal number");
      const p = settle().then(() => op("signal", { signal: signo }));
      p.catch(() => {});
      keep(p.catch(() => {}));
    },
    async setInactivityTimeout(ms: number | bigint): Promise<void> {
      const n = Number(ms);
      if (!Number.isFinite(n) || n < 0) throw new RangeError("setInactivityTimeout: bad duration");
      await settle();
      if (!running) return;
      await op("setInactivityTimeout", { ms: Math.floor(n) });
    },
    async inspect(): Promise<any> {
      await settle();
      const v = await op("inspect");
      return v.info ?? null;
    },
    getTcpPort(port: number) {
      if (!Number.isInteger(port) || port < 1 || port > 65535) throw new RangeError("getTcpPort: bad port");
      return {
        async fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
          await settle();
          const src = new Request(input as any, init);
          // An AbortSignal can't cross the RPC to the platform: send the
          // request without it and honour it here.
          const sig: AbortSignal | undefined = (init as any)?.signal ?? (input instanceof Request ? input.signal : undefined);
          if (sig?.aborted) throw sig.reason ?? new Error("aborted");
          const hasBody = src.method !== "GET" && src.method !== "HEAD";
          const req = new Request(src.url, { method: src.method, headers: src.headers, body: hasBody ? src.body : undefined, redirect: "manual" });
          const pending = tw.ctrFetch({ ...as, port }, req);
          const r: Response = sig
            ? await new Promise<Response>((res, rej) => {
                const onAbort = () => rej(sig.reason ?? new Error("aborted"));
                sig.addEventListener("abort", onAbort, { once: true });
                pending.then(
                  (v) => {
                    sig.removeEventListener("abort", onAbort);
                    res(v);
                  },
                  (e) => {
                    sig.removeEventListener("abort", onAbort);
                    rej(e);
                  },
                );
              })
            : await pending;
          const code = r.headers.get("twd-error");
          if (code && (code === "not_listening" || code === "container_not_running" || code.startsWith("container_"))) {
            throw await errorOf(r);
          }
          return r;
        },
        connect(): never {
          throw Object.assign(new Error("getTcpPort().connect() is not supported on twd yet (ADR-032: deferred)"), { code: "not_supported" });
        },
      };
    },
    async exec(argv: string[], options: any = {}): Promise<any> {
      if (!Array.isArray(argv) || argv.length === 0 || argv.some((a) => typeof a !== "string")) throw new TypeError("exec: argv must be a non-empty string array");
      await settle();
      if (!running) throw startErr ?? new Error(NOT_RUNNING);
      const sig: AbortSignal | undefined = options.signal;
      if (sig?.aborted) throw sig.reason ?? new Error("aborted");
      const ptyOpt = options.pty;
      const pty = !!ptyOpt;
      const stdoutMode = options.stdout ?? "pipe";
      const stderrMode = pty ? "ignore" : (options.stderr ?? "pipe");
      const stdinOpt = options.stdin;
      const execSpec = {
        argv,
        env: options.env,
        cwd: options.cwd,
        user: options.user,
        stdin: stdinOpt !== undefined && stdinOpt !== null,
        stdout: stdoutMode,
        stderr: stderrMode,
        pty,
        cols: typeof ptyOpt === "object" ? ptyOpt.cols : undefined,
        rows: typeof ptyOpt === "object" ? ptyOpt.rows : undefined,
      };
      const r = await tw.ctrExec({ ...as, spec: execSpec });
      if (!r.ok || !r.body) throw await errorOf(r);
      const id = r.headers.get("x-twd-exec") ?? "";
      const pid = Number(r.headers.get("x-twd-exec-pid") ?? 0);
      const { stdout, stderr, exitCode } = demux(r.body, stdoutMode !== "ignore", stderrMode === "pipe");
      let done = false;
      exitCode.then(
        () => (done = true),
        () => (done = true),
      );
      let stdin: WritableStream | null = null;
      if (execSpec.stdin) {
        let src: ReadableStream;
        if (stdinOpt === "pipe") {
          const ts = new TransformStream();
          stdin = ts.writable;
          src = ts.readable;
        } else {
          src = stdinOpt as ReadableStream;
        }
        const p = tw.ctrStdin({ ...as, exec: id }, src).catch(() => {});
        keep(p);
      }
      const kill = (signo = 15) => {
        if (done) return;
        const p = op("exec-signal", { exec: id, signal: signo }).catch(() => {});
        keep(p);
      };
      if (sig) {
        const onAbort = () => kill(9);
        sig.addEventListener("abort", onAbort, { once: true });
        exitCode.finally(() => sig.removeEventListener("abort", onAbort)).catch(() => {});
      }
      return {
        stdin,
        stdout,
        stderr,
        pid,
        isPty: pty,
        exitCode,
        async output() {
          const [o, e, code] = await Promise.all([readAll(stdout), readAll(stderr), exitCode]);
          return { stdout: o, stderr: e, exitCode: code };
        },
        kill,
        resize(cols: number, rows: number) {
          if (!pty) throw new Error("resize: not a PTY exec");
          const p = op("exec-resize", { exec: id, cols, rows }).catch(() => {});
          keep(p);
        },
      };
    },
    // Egress interception registers the handler with hostd's (phase B)
    // egress proxy. Until it exists the container has no egress at all, so
    // nothing reaches the handler (deny-all is the stricter behaviour).
    async interceptOutboundHttp(addr: string, binding: unknown): Promise<void> {
      await settle();
      intercepts.set("http " + addr, binding);
      await op("intercept", { kind: "http", addr: String(addr) });
    },
    async interceptOutboundHttps(addr: string, binding: unknown): Promise<void> {
      await settle();
      intercepts.set("https " + addr, binding);
      await op("intercept", { kind: "https", addr: String(addr) });
    },
    async interceptAllOutboundHttp(binding: unknown): Promise<void> {
      await settle();
      intercepts.set("all *", binding);
      await op("intercept", { kind: "all", addr: "*" });
    },
    async snapshotContainer(): Promise<never> {
      throw Object.assign(new Error("snapshotContainer() is not supported on twd yet (ADR-032 D6: v1.1)"), { code: "not_supported" });
    },
  };
  void cls;
  return Object.freeze(c);
}

export function defineContainer(ctx: any, c: unknown) {
  try {
    Object.defineProperty(ctx, "container", { value: c, enumerable: false, configurable: true });
  } catch {
    /* ignore */
  }
}
