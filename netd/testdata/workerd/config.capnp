# Test platform for netd's hermetic tests. Socket/service addresses are
# overridden on the command line:
#   -s ingress=unix:<rundir>/ingress.sock  -e netd-egress=unix:<rundir>/egress.sock
#   --directory-path do-storage=<rundir>/do
using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "platform", worker = .platform),
    (name = "netd-egress", external = (
      address = "unix:/nonexistent/egress.sock",
      http = (style = proxy),   # absolute-form URLs; netd originates the connection
    )),
    (name = "do-storage", disk = (path = "/nonexistent/do", writable = true)),
  ],
  sockets = [
    (name = "ingress", address = "unix:/nonexistent/ingress.sock", service = "platform",
     http = (cfBlobHeader = "x-twd-cf")),   # stripped by workerd; JSON becomes request.cf
  ],
);

const platform :Workerd.Worker = (
  compatibilityDate = "2026-04-01",
  modules = [
    (name = "platform.js", esModule = embed "platform.js"),
    (name = "tenant.js.txt", text = embed "tenant.js"),
  ],
  durableObjectNamespaces = [ (className = "HostDO", uniqueKey = "twd-hostdo", enableSql = true) ],
  durableObjectStorage = (localDisk = "do-storage"),
  bindings = [
    (name = "LOADER", workerLoader = ()),
    (name = "NETD_EGRESS", service = "netd-egress"),
  ],
  globalOutbound = "netd-egress",
);
