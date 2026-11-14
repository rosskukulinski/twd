package vm

import (
	"fmt"
	"os"
)

// smokeCapnp is the smoke tests' workerd config: w.js on the guest's
// ingress, and one outbound service through the gateway (attributed egress).
const smokeCapnp = `using Workerd = import "/workerd/workerd.capnp";
const config :Workerd.Config = (
  services = [ (name = "main", worker = .w), (name = "gw", external = (address = "192.168.72.1:3128", http = ())) ],
  sockets = [ (name = "in", address = "*:8080", service = "main", http = (cfBlobHeader = "x-twd-cf")) ],
);
const w :Workerd.Worker = (
  compatibilityDate = "2026-04-01",
  modules = [ (name = "w.js", esModule = embed "w.js") ],
  bindings = [ (name = "GW", service = "gw") ],
);
`

func envInt(k string) int {
	var n int
	fmt.Sscan(os.Getenv(k), &n)
	return n
}
