package amneziawgkern

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawg"
)

// installCommand is what an admin runs to satisfy every host-level blocker at
// once. It names the running binary because /usr/bin/x-ui is the management
// shell script, which has no such subcommand and would just open its menu.
func installCommand() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "sudo /usr/local/x-ui/x-ui install-awg-kernel"
	}
	return "sudo " + exe + " install-awg-kernel"
}

// BlockerCode identifies one reason kernel mode cannot run. The UI translates
// off the code, so the panel never has to render an English string that came
// from the server.
type BlockerCode string

const (
	BlockerNotRoot       BlockerCode = "root"
	BlockerToolsMissing  BlockerCode = "tools"
	BlockerModuleMissing BlockerCode = "module"
	BlockerNftMissing    BlockerCode = "nft"
	BlockerIPv6          BlockerCode = "ipv6"
	BlockerPortForward   BlockerCode = "portForwarding"
	BlockerRouteXray     BlockerCode = "routeThroughXray"
	BlockerListenAddress BlockerCode = "listenAddress"
)

// Blocker is one unmet requirement. Detail carries the specifics the code
// alone cannot express (which binary is missing, say) and is meant to be shown
// alongside the translated text, not instead of it.
type Blocker struct {
	Code   BlockerCode `json:"code"`
	Detail string      `json:"detail,omitempty"`
}

// Readiness is the host's ability to run any kernel-mode inbound at all.
type Readiness struct {
	Ready          bool      `json:"ready"`
	Blockers       []Blocker `json:"blockers"`
	InstallCommand string    `json:"installCommand"`
}

// readinessTTL keeps the form's polling from re-running modprobe on every
// open. Short enough that installing the module shows up almost immediately.
const readinessTTL = 10 * time.Second

var (
	readinessMu     sync.Mutex
	readinessCached Readiness
	readinessAt     time.Time
)

// HostReadiness reports whether this host can run kernel mode, independent of
// any particular inbound -- the UI has to answer that before an instance
// exists. Cached for readinessTTL because it probes the module.
func HostReadiness() Readiness {
	readinessMu.Lock()
	defer readinessMu.Unlock()

	if !readinessAt.IsZero() && time.Since(readinessAt) < readinessTTL {
		return readinessCached
	}
	readinessCached = probeHost()
	readinessAt = time.Now()
	return readinessCached
}

// resetReadinessCache exists for tests; production code has no reason to force
// a re-probe.
func resetReadinessCache() {
	readinessMu.Lock()
	defer readinessMu.Unlock()
	readinessAt = time.Time{}
	readinessCached = Readiness{}
}

func probeHost() Readiness {
	r := Readiness{InstallCommand: installCommand()}

	if os.Geteuid() != 0 {
		r.Blockers = append(r.Blockers, Blocker{Code: BlockerNotRoot})
	}
	for _, bin := range []string{"awg", "awg-quick"} {
		if _, err := exec.LookPath(bin); err != nil {
			r.Blockers = append(r.Blockers, Blocker{Code: BlockerToolsMissing, Detail: bin})
		}
	}
	if !moduleAvailable() {
		r.Blockers = append(r.Blockers, Blocker{Code: BlockerModuleMissing})
	}
	// NAT is the only egress kernel mode implements today, so nft is required
	// unconditionally. When the TPROXY bridge lands this becomes conditional
	// on routeThroughXray.
	if _, err := exec.LookPath("nft"); err != nil {
		r.Blockers = append(r.Blockers, Blocker{Code: BlockerNftMissing})
	}

	r.Ready = len(r.Blockers) == 0
	return r
}

// Preflight reports every reason this host or this inbound cannot run on the
// kernel backend, all at once rather than one failure per save attempt: an
// admin flipping the toggle wants the full list of what to fix, not a
// six-round game of fix-and-retry.
//
// The UI gates the toggle on HostReadiness, but this still runs on save. The
// UI's view can be stale, the API can be called directly, and a host can lose
// the module between the form loading and the save landing.
func Preflight(inst amneziawg.Instance, routeThroughXray bool) error {
	blockers := append([]Blocker(nil), HostReadiness().Blockers...)

	// Features the embedded backend has and this one does not yet. Refusing up
	// front beats silently dropping them: a peer whose port-forward stops
	// working after a backend switch is a support ticket nobody can diagnose.
	if instanceWantsIPv6(inst) {
		blockers = append(blockers, Blocker{Code: BlockerIPv6})
	}
	if instanceWantsPortForwarding(inst) {
		blockers = append(blockers, Blocker{Code: BlockerPortForward})
	}
	// Render drops the whole NAT block for routeThroughXray and no TPROXY bridge
	// replaces it, so the tunnel would come up with no egress path at all.
	if routeThroughXray {
		blockers = append(blockers, Blocker{Code: BlockerRouteXray})
	}
	if listenNeedsPinnedBind(inst.Listen) {
		blockers = append(blockers, Blocker{Code: BlockerListenAddress, Detail: inst.Listen})
	}

	if len(blockers) == 0 {
		return nil
	}
	return fmt.Errorf("kernel mode unavailable: %s", strings.Join(describe(blockers), "; "))
}

// describe renders blockers for the error a direct API caller sees. The panel
// UI never reaches this path -- it translates the codes itself.
func describe(blockers []Blocker) []string {
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		switch b.Code {
		case BlockerNotRoot:
			out = append(out, "the panel is not running as root, so it cannot create a network interface")
		case BlockerToolsMissing:
			out = append(out, fmt.Sprintf("%s not found in PATH (install amneziawg-tools)", b.Detail))
		case BlockerModuleMissing:
			out = append(out, "the amneziawg kernel module is not loaded (run "+installCommand()+")")
		case BlockerNftMissing:
			out = append(out, "nft not found in PATH (install nftables); it is needed for the tunnel's NAT rules")
		case BlockerIPv6:
			out = append(out, "per-peer IPv6 egress is not supported in kernel mode yet; disable IPv6 on this inbound first")
		case BlockerPortForward:
			out = append(out, "per-client port forwarding is not supported in kernel mode yet; clear the forwarded ports first")
		case BlockerRouteXray:
			out = append(out, "routeThroughXray has no kernel-mode implementation; it would leave the tunnel with no egress path -- turn it off first")
		case BlockerListenAddress:
			out = append(out, fmt.Sprintf("kernel mode cannot bind to the configured listen address %q; clear it or use a wildcard", b.Detail))
		default:
			out = append(out, string(b.Code))
		}
	}
	return out
}

// moduleAvailable checks for a loaded amneziawg module, then tries to load it.
// Both paths matter: on a freshly booted host nothing has referenced the
// module yet, and awg-quick would load it implicitly anyway -- doing it here
// turns "interface failed to come up" into a precise message at save time.
func moduleAvailable() bool {
	if _, err := os.Stat("/sys/module/amneziawg"); err == nil {
		return true
	}
	if _, err := exec.LookPath("modprobe"); err != nil {
		return false
	}
	if _, err := run("modprobe", "amneziawg"); err != nil {
		return false
	}
	_, err := os.Stat("/sys/module/amneziawg")
	return err == nil
}

func instanceWantsIPv6(inst amneziawg.Instance) bool {
	if inst.IPv6Enabled {
		return true
	}
	for _, a := range inst.Address {
		if strings.Contains(a, ":") {
			return true
		}
	}
	for _, p := range inst.Peers {
		for _, a := range p.AllowedIPs {
			if strings.Contains(a, ":") {
				return true
			}
		}
	}
	return false
}

func instanceWantsPortForwarding(inst amneziawg.Instance) bool {
	for _, p := range inst.Peers {
		if strings.TrimSpace(p.ForwardedPorts) != "" {
			return true
		}
	}
	return false
}
