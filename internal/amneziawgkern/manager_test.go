package amneziawgkern

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawg"
)

// TestStructuralFingerprintIgnoresPeers is what keeps live sessions alive: if
// adding a client changed the fingerprint, every admin edit would take the
// interface down and back up and reset every peer's handshake.
func TestStructuralFingerprintIgnoresPeers(t *testing.T) {
	t.Parallel()

	base := testInstance()
	withPeer := testInstance()
	withPeer.Peers = append(withPeer.Peers, amneziawg.Peer{
		PublicKey:  "peerCPublicKeyBase64ValueForTestsPadding000=",
		AllowedIPs: []string{"10.8.1.4/32"},
	})

	a, err := Render(base, "eth0", false)
	if err != nil {
		t.Fatalf("Render(base): %v", err)
	}
	b, err := Render(withPeer, "eth0", false)
	if err != nil {
		t.Fatalf("Render(withPeer): %v", err)
	}
	if a == b {
		t.Fatal("adding a peer must change the rendered config")
	}
	if structuralFingerprint(a) != structuralFingerprint(b) {
		t.Error("adding a peer must not change the structural fingerprint, or syncconf is never used")
	}
}

// TestStructuralFingerprintCatchesInterfaceChanges: these are the properties
// the kernel fixes when the interface is created, so missing one here would
// leave the live interface permanently out of sync with its config.
func TestStructuralFingerprintCatchesInterfaceChanges(t *testing.T) {
	t.Parallel()

	base, err := Render(testInstance(), "eth0", false)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	baseFP := structuralFingerprint(base)

	cases := map[string]func(*amneziawg.Instance){
		"listen port": func(i *amneziawg.Instance) { i.ListenPort = 40000 },
		"mtu":         func(i *amneziawg.Instance) { i.MTU = 1280 },
		"address":     func(i *amneziawg.Instance) { i.Address = []string{"10.9.9.1/24"} },
		"private key": func(i *amneziawg.Instance) { i.PrivateKey = "otherPrivateKeyBase64ValueForTestsPadding00=" },
		"obfuscation": func(i *amneziawg.Instance) { i.Obfuscation.S4 = 21 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			inst := testInstance()
			mutate(&inst)
			got, err := Render(inst, "eth0", false)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if structuralFingerprint(got) == baseFP {
				t.Errorf("changing the %s must change the structural fingerprint", name)
			}
		})
	}
}

// TestStructuralFingerprintCatchesEgressInterface: the NAT block lives in the
// interface section, and a masquerade pointing at the wrong NIC black-holes
// every peer.
func TestStructuralFingerprintCatchesEgressInterface(t *testing.T) {
	t.Parallel()

	a, err := Render(testInstance(), "eth0", false)
	if err != nil {
		t.Fatalf("Render(eth0): %v", err)
	}
	b, err := Render(testInstance(), "ens3", false)
	if err != nil {
		t.Fatalf("Render(ens3): %v", err)
	}
	if structuralFingerprint(a) == structuralFingerprint(b) {
		t.Error("changing the egress interface must force a rebuild, not a peer sync")
	}
}

func TestInterfaceNameFallsBackToInboundID(t *testing.T) {
	t.Parallel()

	inst := testInstance()
	inst.InterfaceName = ""
	inst.Id = 42
	if got, want := interfaceName(inst), "awg42"; got != want {
		t.Errorf("interfaceName = %q, want %q", got, want)
	}

	inst.InterfaceName = "awgcustom"
	if got, want := interfaceName(inst), "awgcustom"; got != want {
		t.Errorf("interfaceName = %q, want %q", got, want)
	}
}

// TestWriteConfIsPrivateAndComplete: awg-quick refuses a world-readable
// config, and the file holds the interface private key.
func TestWriteConfIsPrivateAndComplete(t *testing.T) {
	dir := t.TempDir()

	conf, err := Render(testInstance(), "eth0", false)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := writeConf(dir, "awgtest", conf); err != nil {
		t.Fatalf("writeConf: %v", err)
	}

	path := filepath.Join(dir, "awgtest.conf")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	if string(got) != conf {
		t.Error("written config does not match what was rendered")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("config mode = %o, want 600", perm)
		}
	}

	// Rewriting must replace, not append or leave a temp file behind.
	if err := writeConf(dir, "awgtest", conf); err != nil {
		t.Fatalf("second writeConf: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only the config to remain, got %v", names)
	}
}

// TestAddressesSatisfiedIsASubsetCheck: the kernel adds an IPv6 link-local
// address nobody configured, so requiring an exact match would make every
// interface look wrong and force a needless rebuild on every panel restart.
func TestAddressesSatisfiedIsASubsetCheck(t *testing.T) {
	t.Parallel()

	live := []string{"10.8.1.1/24", "fe80::be24:11ff:fe3f:5f75/64"}
	if !addressesSatisfied([]string{"10.8.1.1/24"}, live) {
		t.Error("an extra kernel-added address must not disqualify the interface")
	}
	if addressesSatisfied([]string{"10.9.9.1/24"}, live) {
		t.Error("a different address must disqualify the interface")
	}
	if addressesSatisfied([]string{"10.8.1.1/24", "fd00::1/64"}, live) {
		t.Error("a missing wanted address must disqualify the interface")
	}
	if addressesSatisfied(nil, live) {
		t.Error("an instance with no address must never be adopted")
	}
}

// TestLiveInterfaceMatchesRejectsMissingLink is the guard that keeps adoption
// from claiming an interface that is not there.
func TestLiveInterfaceMatchesRejectsMissingLink(t *testing.T) {
	t.Parallel()

	if liveInterfaceMatches("awg-does-not-exist-9999", testInstance()) {
		t.Error("a nonexistent interface must never be adopted")
	}
}

// TestStopAllLeavesInterfacesRunning: the kernel datapath keeps forwarding
// while the panel is down, so shutdown must only drop bookkeeping. Tearing the
// tunnel down here would disconnect every user for the length of an upgrade
// and leave the idle ones off afterwards.
func TestStopAllLeavesInterfacesRunning(t *testing.T) {
	m := &Manager{ifaces: map[int]*managed{
		1: {name: "awg-test-never-created", conf: "x", structFP: "y"},
	}, confDir: t.TempDir()}

	m.StopAll()

	if m.HasRunning() {
		t.Error("StopAll must clear the manager's bookkeeping")
	}
	// teardown() would have run awg-quick against a nonexistent link; the point
	// of the assertion above is that the map is cleared without touching it.
}
