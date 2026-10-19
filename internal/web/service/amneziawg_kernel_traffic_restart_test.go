package service

import (
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawgkern"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
)

func TestKernelTrafficCollectorRestartDoesNotRechargeCounters(t *testing.T) {
	db := initTrafficTestDB(t)
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

	const iface = "awg-1"
	const publicKey = "peer-public-key"
	service := &InboundService{}
	firstProcess := amneziawgkern.NewCollector()
	record := func(collector *amneziawgkern.Collector, rx, tx int64) {
		t.Helper()
		for _, delta := range collector.Delta(iface, []amneziawgkern.PeerStat{{
			PublicKey: publicKey, RxBytes: rx, TxBytes: tx,
		}}) {
			if _, _, err := service.AddTraffic(nil, []*xray.ClientTraffic{{
				InboundId: inbound.Id,
				Email:     email,
				Up:        delta.Up,
				Down:      delta.Down,
			}}); err != nil {
				t.Fatalf("record kernel traffic: %v", err)
			}
		}
	}

	// The first process has already billed these cumulative kernel counters.
	record(firstProcess, 100, 200)
	// The interface survives the panel restart and accrues only 25/50 more bytes.
	// A fresh Collector currently treats the entire 125/250 total as new traffic.
	restartedProcess := amneziawgkern.NewCollector()
	record(restartedProcess, 125, 250)

	var got xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&got).Error; err != nil {
		t.Fatalf("reload client traffic: %v", err)
	}
	if got.Up != 125 || got.Down != 250 {
		t.Fatalf("usage after adopted-interface restart = up %d down %d, want 125/250 (including only the 25/50 bytes accrued after the previous poll)", got.Up, got.Down)
	}
}
