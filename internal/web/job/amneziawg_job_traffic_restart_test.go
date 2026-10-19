package job

import (
	"errors"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawg"
	"github.com/mhsanaei/3x-ui/v3/internal/amneziawgkern"
	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/web/service"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
)

type kernelTrafficTestManager struct {
	state amneziawgkern.TrafficInterfaceState
}

func (m *kernelTrafficTestManager) TrafficStateFor(int) (amneziawgkern.TrafficInterfaceState, bool) {
	return m.state, true
}

func (m *kernelTrafficTestManager) PrimeAdoptedTrafficBaseline(_ int, generation uint64, stats []amneziawgkern.PeerStat, collector *amneziawgkern.Collector) bool {
	if !m.state.Adopted || m.state.Generation != generation {
		return false
	}
	collector.Prime(m.state.Name, stats)
	m.state.Adopted = false
	return true
}

func TestKernelTrafficJobDoesNotRechargeAfterAdoptingInterface(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	inbound := &model.Inbound{
		UserId: 1, Tag: "kernel-restart", Enable: true, Port: 45201,
		Protocol: model.AmneziaWG, Settings: `{"clients":[]}`,
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	const email = "kernel-restart@example.test"
	if err := db.Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: email, Enable: true}).Error; err != nil {
		t.Fatalf("create client traffic: %v", err)
	}

	const (
		iface     = "awg-restart"
		publicKey = "peer-public-key"
	)
	desired := []amneziawgkern.Desired{{Instance: amneziawg.Instance{
		Id: inbound.Id, Peers: []amneziawg.Peer{{Email: email, PublicKey: publicKey}},
	}}}
	firstCollector := amneziawgkern.NewCollector()
	firstManager := &kernelTrafficTestManager{state: amneziawgkern.TrafficInterfaceState{Name: iface, Generation: 1}}
	current := []amneziawgkern.PeerStat{{PublicKey: publicKey, RxBytes: 100, TxBytes: 200}}
	collect := func(manager *kernelTrafficTestManager, collector *amneziawgkern.Collector, dump kernelTrafficDumper) {
		t.Helper()
		collectKernelTrafficWith(&service.InboundService{}, desired, manager, collector, dump)
	}
	collect(firstManager, firstCollector, func(string) ([]amneziawgkern.PeerStat, error) { return current, nil })

	// The panel restarts while the kernel interface keeps its cumulative counters.
	restartedCollector := amneziawgkern.NewCollector()
	restartedManager := &kernelTrafficTestManager{state: amneziawgkern.TrafficInterfaceState{
		Name: iface, Generation: 2, Adopted: true,
	}}
	collect(restartedManager, restartedCollector, func(string) ([]amneziawgkern.PeerStat, error) {
		return nil, errors.New("temporary dump error")
	})
	collect(restartedManager, restartedCollector, func(string) ([]amneziawgkern.PeerStat, error) { return nil, nil })
	if !restartedManager.state.Adopted {
		t.Fatal("failed and empty dumps consumed the adopted baseline")
	}
	current = []amneziawgkern.PeerStat{{PublicKey: publicKey, RxBytes: 125, TxBytes: 250}}
	collect(restartedManager, restartedCollector, func(string) ([]amneziawgkern.PeerStat, error) { return current, nil })
	var afterBaseline xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&afterBaseline).Error; err != nil {
		t.Fatalf("reload traffic after baseline: %v", err)
	}
	if afterBaseline.Up != 100 || afterBaseline.Down != 200 {
		t.Fatalf("first adopted dump charged traffic: up %d down %d, want unchanged 100/200", afterBaseline.Up, afterBaseline.Down)
	}
	if restartedManager.state.Adopted {
		t.Fatal("successful nonempty dump did not consume the adopted baseline")
	}
	current = []amneziawgkern.PeerStat{{PublicKey: publicKey, RxBytes: 150, TxBytes: 300}}
	collect(restartedManager, restartedCollector, func(string) ([]amneziawgkern.PeerStat, error) { return current, nil })

	var got xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&got).Error; err != nil {
		t.Fatalf("reload client traffic: %v", err)
	}
	if got.Up != 125 || got.Down != 250 {
		t.Fatalf("usage after adopted-interface restart = up %d down %d, want 125/250 (100/200 before restart plus 25/50 after the baseline)", got.Up, got.Down)
	}

	const newEmail = "kernel-new-peer@example.test"
	if err := db.Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: newEmail, Enable: true}).Error; err != nil {
		t.Fatalf("create new peer traffic: %v", err)
	}
	const newPublicKey = "new-peer-public-key"
	desired[0].Instance.Peers = append(desired[0].Instance.Peers, amneziawg.Peer{Email: newEmail, PublicKey: newPublicKey})
	current = []amneziawgkern.PeerStat{
		{PublicKey: publicKey, RxBytes: 150, TxBytes: 300},
		{PublicKey: newPublicKey, RxBytes: 7, TxBytes: 9},
	}
	collect(restartedManager, restartedCollector, func(string) ([]amneziawgkern.PeerStat, error) { return current, nil })
	var newPeerTraffic xray.ClientTraffic
	if err := db.Where("email = ?", newEmail).First(&newPeerTraffic).Error; err != nil {
		t.Fatalf("reload new peer traffic: %v", err)
	}
	if newPeerTraffic.Up != 7 || newPeerTraffic.Down != 9 {
		t.Fatalf("new peer first counters = up %d down %d, want 7/9", newPeerTraffic.Up, newPeerTraffic.Down)
	}
}
