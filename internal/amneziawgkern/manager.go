package amneziawgkern

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawg"
	"github.com/mhsanaei/3x-ui/v3/internal/logger"
)

// commandTimeout bounds every awg-quick/awg/ip invocation. awg-quick runs the
// PostUp block synchronously, so this has to cover nftables work too, not just
// the netlink calls.
const commandTimeout = 20 * time.Second

// Desired is one inbound's wanted kernel-interface state.
type Desired struct {
	Instance amneziawg.Instance
	// EgressInterface is the host NIC masquerade attaches to; blank means
	// detect it (DetectDefaultInterface).
	EgressInterface string
	// RouteThroughXray selects the TPROXY bridge instead of plain NAT, the
	// same opt-in the retired kernel architecture exposed.
	RouteThroughXray bool
}

// managed is one live interface plus enough state to decide, on the next
// reconcile tick, whether anything actually has to be done.
type managed struct {
	name string
	// conf is the full rendered config last written, so an unchanged tick is
	// a pure string compare with no exec at all.
	conf string
	// structFP covers everything awg syncconf cannot change on a live
	// interface -- address, MTU, listen port, key, obfuscation, NAT block.
	// When only the peer list differs, syncconf keeps existing sessions
	// alive; a structural change has to go down and back up.
	structFP string
}

// Manager owns the running kernel interfaces keyed by inbound id, mirroring
// internal/amneziawgnet.Manager's shape so callers can hold either behind the
// same call sites.
type Manager struct {
	mu     sync.Mutex
	ifaces map[int]*managed
	// confDir is where awg-quick configs are written; a field rather than a
	// constant so the write path is testable off /etc.
	confDir string
}

var (
	managerOnce sync.Once
	manager     *Manager
)

// GetManager returns the process-wide kernel-AmneziaWG manager singleton.
func GetManager() *Manager {
	managerOnce.Do(func() {
		manager = &Manager{ifaces: map[int]*managed{}, confDir: ConfDir}
	})
	return manager
}

// Ensure brings the inbound's kernel interface to the state d describes.
//
// Three outcomes, in the order they are cheapest: the rendered config is
// byte-identical to what is already applied and nothing runs at all; only the
// peer list changed and `awg syncconf` updates it without disturbing live
// handshakes; or something the kernel fixes at interface-creation time
// changed and the interface is torn down and recreated. That middle branch is
// the whole reason syncconf exists -- `awg setconf` would drop and re-add
// every peer, resetting each live session on every admin edit.
func (m *Manager) Ensure(d Desired) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLocked(d)
}

func (m *Manager) ensureLocked(d Desired) error {
	inst := d.Instance
	name := interfaceName(inst)

	conf, err := Render(inst, d.EgressInterface, d.RouteThroughXray)
	if err != nil {
		return err
	}
	fp := structuralFingerprint(conf)

	cur, exists := m.ifaces[inst.Id]
	if exists && cur.conf == conf && linkExists(name) {
		return nil
	}

	if err := writeConf(m.confDir, name, conf); err != nil {
		return err
	}

	// Adopt an interface that outlived the panel. After a restart m.ifaces is
	// empty, so without this every start tore down a perfectly good tunnel and
	// dropped every peer's session -- and an idle client with no
	// PersistentKeepalive has nothing to trigger a reconnect, so it simply
	// stays gone. Address and MTU are checked against the live netdev because
	// they are the only properties syncconf cannot repair; keys, listen port
	// and every obfuscation field it applies for us.
	if !exists && liveInterfaceMatches(name, inst) {
		if err := m.syncPeers(name); err == nil {
			m.ifaces[inst.Id] = &managed{name: name, conf: conf, structFP: fp}
			logger.Info("amneziawgkern: adopted running interface ", name, " for inbound ", inst.Id)
			return nil
		} else {
			logger.Warning("amneziawgkern: could not adopt ", name, ", rebuilding: ", err)
		}
	}

	// Reconfigure in place only when we know the interface is genuinely ours
	// and structurally unchanged. Anything else -- first Ensure after a panel
	// restart, a link that vanished underneath us, a changed port or key --
	// goes through a full down/up so the kernel state cannot silently diverge
	// from the config on disk.
	if exists && cur.structFP == fp && linkExists(name) {
		if err := m.syncPeers(name); err != nil {
			return err
		}
		m.ifaces[inst.Id] = &managed{name: name, conf: conf, structFP: fp}
		logger.Debug("amneziawgkern: synced peers for ", name)
		return nil
	}

	if linkExists(name) {
		if out, err := run("awg-quick", "down", name); err != nil {
			// A stale link with no matching config makes awg-quick down fail;
			// drop the netdev directly so `up` below starts from nothing.
			logger.Warning("amneziawgkern: awg-quick down ", name, ": ", err, " ", out)
			if out, err := run("ip", "link", "del", name); err != nil {
				return fmt.Errorf("amneziawgkern: removing stale link %s: %w (%s)", name, err, out)
			}
		}
	}
	if out, err := run("awg-quick", "up", name); err != nil {
		return fmt.Errorf("amneziawgkern: awg-quick up %s: %w (%s)", name, err, out)
	}

	m.ifaces[inst.Id] = &managed{name: name, conf: conf, structFP: fp}
	logger.Info("amneziawgkern: started kernel interface ", name, " for inbound ", inst.Id)
	return nil
}

// syncPeers applies the on-disk config's peer list to the live interface.
// awg-quick's own `strip` is what turns an awg-quick config (Address, MTU,
// PostUp) into the subset `awg` accepts -- confirmed against the running
// module: feeding it an unstripped config fails with "Line unrecognized:
// `Address=...'".
func (m *Manager) syncPeers(name string) error {
	stripped, err := run("awg-quick", "strip", ConfPathIn(m.confDir, name))
	if err != nil {
		return fmt.Errorf("amneziawgkern: awg-quick strip %s: %w (%s)", name, err, stripped)
	}
	tmp, err := os.CreateTemp("", "awgsync-*.conf")
	if err != nil {
		return fmt.Errorf("amneziawgkern: temp file for syncconf: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(stripped); err != nil {
		tmp.Close()
		return fmt.Errorf("amneziawgkern: writing syncconf input: %w", err)
	}
	tmp.Close()

	if out, err := run("awg", "syncconf", name, tmp.Name()); err != nil {
		return fmt.Errorf("amneziawgkern: awg syncconf %s: %w (%s)", name, err, out)
	}
	return nil
}

// Reconcile brings the whole set of kernel interfaces to what desired
// describes, tearing down any that are no longer wanted. Mirrors
// amneziawgnet.Manager.Reconcile so one job can drive both backends the same
// way.
func (m *Manager) Reconcile(desired []Desired) {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[int]struct{}, len(desired))
	for _, d := range desired {
		want[d.Instance.Id] = struct{}{}
	}
	for id, cur := range m.ifaces {
		if _, ok := want[id]; ok {
			continue
		}
		if err := teardown(cur.name); err != nil {
			logger.Warning("amneziawgkern: stopping removed inbound ", id, ": ", err)
		}
		GetCollector().Forget(cur.name)
		delete(m.ifaces, id)
		logger.Info("amneziawgkern: stopped kernel interface for removed inbound ", id)
	}
	for _, d := range desired {
		if err := m.ensureLocked(d); err != nil {
			logger.Warning("amneziawgkern: ensure inbound ", d.Instance.Id, ": ", err)
		}
	}
}

// Remove tears down one inbound's interface and forgets it.
func (m *Manager) Remove(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur, ok := m.ifaces[id]
	if !ok {
		return nil
	}
	delete(m.ifaces, id)
	GetCollector().Forget(cur.name)
	return teardown(cur.name)
}

// StopAll releases this process's bookkeeping at panel shutdown and
// deliberately leaves the kernel interfaces running.
//
// The whole point of the kernel datapath is that forwarding does not live in
// this process: peers keep passing traffic while the panel restarts, exactly
// as a plain awg-quick service would. Tearing the tunnels down here would cut
// every user off for the duration of a panel upgrade, and -- because an idle
// peer with no PersistentKeepalive has nothing to trigger a reconnect -- some
// of them would simply stay off afterwards. The next start adopts whatever is
// still up (see ensureLocked); an inbound that is genuinely removed or
// disabled is torn down by Remove/Reconcile, which is where that belongs.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, cur := range m.ifaces {
		logger.Info("amneziawgkern: leaving kernel interface ", cur.name, " up across shutdown")
		delete(m.ifaces, id)
	}
}

// HasRunning reports whether any kernel interface is currently managed.
func (m *Manager) HasRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ifaces) > 0
}

// Running reports whether this inbound has a live kernel interface.
func (m *Manager) Running(id int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.ifaces[id]
	return ok && linkExists(cur.name)
}

// InterfaceNameFor exposes the interface an inbound would use, for callers
// that need to read counters or report state without holding a Desired.
func (m *Manager) InterfaceNameFor(id int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.ifaces[id]
	if !ok {
		return "", false
	}
	return cur.name, true
}

// liveInterfaceMatches reports whether an existing netdev is already carrying
// the address and MTU inst wants, i.e. whether it can be adopted instead of
// recreated. Deliberately stdlib-only: this runs on every reconcile tick, and
// shelling out to `ip` for something the kernel already exposes would be waste.
func liveInterfaceMatches(name string, inst amneziawg.Instance) bool {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	if ifi.MTU != effectiveMTU(inst) {
		return false
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	live := make([]string, 0, len(addrs))
	for _, a := range addrs {
		live = append(live, a.String())
	}
	return addressesSatisfied(inst.Address, live)
}

// addressesSatisfied reports whether every wanted address is present on the
// interface. A subset check, not equality: the kernel adds an IPv6 link-local
// address of its own that no config ever asks for.
func addressesSatisfied(wanted, live []string) bool {
	if len(wanted) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(live))
	for _, a := range live {
		have[normalizeCIDR(a)] = struct{}{}
	}
	for _, w := range wanted {
		if _, ok := have[normalizeCIDR(w)]; !ok {
			return false
		}
	}
	return true
}

// normalizeCIDR makes "10.8.1.1/24" comparable however it was spelled.
func normalizeCIDR(s string) string {
	s = strings.TrimSpace(s)
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return s
	}
	return p.String()
}

func teardown(name string) error {
	if !linkExists(name) {
		return nil
	}
	if out, err := run("awg-quick", "down", name); err != nil {
		return fmt.Errorf("amneziawgkern: awg-quick down %s: %w (%s)", name, err, out)
	}
	return nil
}

// interfaceName falls back to awg<id> so two inbounds can never collide, and
// so the name never clashes with whatever the embedded backend calls its own.
func interfaceName(inst amneziawg.Instance) string {
	if n := strings.TrimSpace(inst.InterfaceName); n != "" {
		return n
	}
	return fmt.Sprintf("awg%d", inst.Id)
}

// structuralFingerprint hashes everything above the first [Peer] section --
// exactly the properties awg syncconf cannot change on a live interface.
func structuralFingerprint(conf string) string {
	head := conf
	if i := strings.Index(conf, "\n[Peer]"); i >= 0 {
		head = conf[:i]
	}
	sum := sha256.Sum256([]byte(head))
	return hex.EncodeToString(sum[:])
}

// writeConf replaces the interface's config atomically at 0600: it holds a
// private key, and awg-quick refuses a world-readable config.
func writeConf(dir, name, conf string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("amneziawgkern: creating %s: %w", dir, err)
	}
	path := ConfPathIn(dir, name)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".awgconf-*")
	if err != nil {
		return fmt.Errorf("amneziawgkern: temp config: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(conf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("amneziawgkern: writing config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("amneziawgkern: chmod config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("amneziawgkern: closing config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("amneziawgkern: installing config at %s: %w", path, err)
	}
	return nil
}

func linkExists(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name)
	return err == nil
}

// run executes one command with a bounded timeout and returns its combined
// output, which is where awg-quick puts the detail worth logging on failure.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
