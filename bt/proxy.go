package bt

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	api "pixeld/esphomeapi"
)

// ESPHome's Bluetooth proxy feature flags (bluetooth_proxy.h): what Home Assistant may use.
const (
	featPassiveScan  = 1 << 0
	featRawAdverts   = 1 << 5
	featStateAndMode = 1 << 6

	batchMax   = 16                     // advertisements per message, as ESPHome sends them
	batchEvery = 100 * time.Millisecond // ... or whatever has come in by then
	// ESPHome's proxy default: a 30 ms window every 320 ms, leaving the shared radio to WiFi (audio) most
	// of the time.
	scanInterval = 512 // × 0.625 ms
	scanWindow   = 48
)

// Proxy relays BLE advertisements to the Home Assistant connections subscribed to them.
type Proxy struct {
	mu      sync.Mutex
	h       *hci
	mac     string
	subs    map[*api.Conn]bool
	active  bool // scan mode Home Assistant asked for
	running bool
	failed  bool
	batch   []*api.BluetoothLERawAdvertisement
}

// Start brings the controller up and opens it; the proxy then scans whenever Home Assistant subscribes.
func Start(ctx context.Context) (*Proxy, error) {
	if err := powerUp(ctx); err != nil {
		return nil, err
	}
	p := &Proxy{subs: map[*api.Conn]bool{}}
	h, err := openHCI(p.advert)
	if err != nil {
		return nil, err
	}
	mac, err := h.setup()
	if err != nil {
		h.close()
		return nil, err
	}
	p.h, p.mac = h, mac
	go p.flushLoop(ctx)
	go func() {
		<-ctx.Done()
		h.scan(false, false, 0, 0)
		h.close()
	}()
	logf("controller %s ready", mac)
	return p, nil
}

// Flags and MAC go in the ESPHome device info.
func (p *Proxy) Flags() uint32 { return featPassiveScan | featRawAdverts | featStateAndMode }
func (p *Proxy) MAC() string   { return p.mac }

// Message handles the Bluetooth proxy messages (reporting whether m was one).
func (p *Proxy) Message(c *api.Conn, m proto.Message) bool {
	switch req := m.(type) {
	case *api.SubscribeBluetoothLEAdvertisementsRequest:
		p.mu.Lock()
		p.subs[c] = true
		p.mu.Unlock()
		p.apply()
		c.Send(p.state())
	case *api.UnsubscribeBluetoothLEAdvertisementsRequest:
		p.Disconnected(c)
	case *api.BluetoothScannerSetModeRequest:
		p.mu.Lock()
		p.active = req.Mode == api.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE
		restart := p.running
		p.running = false
		p.mu.Unlock()
		if restart {
			p.apply()
		}
		p.sendAll(p.state())
	default:
		return false
	}
	return true
}

// Disconnected drops a connection's subscription (scanning stops with the last one).
func (p *Proxy) Disconnected(c *api.Conn) {
	p.mu.Lock()
	delete(p.subs, c)
	p.mu.Unlock()
	p.apply()
}

// apply scans while anyone is subscribed, in the mode asked for.
func (p *Proxy) apply() {
	p.mu.Lock()
	want := len(p.subs) > 0
	running, active := p.running, p.active
	p.mu.Unlock()
	if want == running {
		return
	}
	err := p.h.scan(want, active, scanInterval, scanWindow)
	p.mu.Lock()
	p.failed = err != nil
	if err == nil {
		p.running = want
	}
	p.mu.Unlock()
	if err != nil {
		logf("scan: %v", err)
	} else if want {
		mode := "passive"
		if active {
			mode = "active"
		}
		logf("scanning (%s)", mode)
	} else {
		logf("scanning stopped")
	}
}

func (p *Proxy) state() *api.BluetoothScannerStateResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := api.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_IDLE
	switch {
	case p.failed:
		st = api.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_FAILED
	case p.running:
		st = api.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING
	}
	mode := api.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE
	if p.active {
		mode = api.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE
	}
	return &api.BluetoothScannerStateResponse{State: st, Mode: mode, ConfiguredMode: mode}
}

func (p *Proxy) advert(r report) {
	a := &api.BluetoothLERawAdvertisement{Address: r.addr, Rssi: int32(r.rssi), AddressType: uint32(r.addrType),
		Data: r.data}
	p.mu.Lock()
	p.batch = append(p.batch, a)
	full := len(p.batch) >= batchMax
	p.mu.Unlock()
	if full {
		p.flush()
	}
}

func (p *Proxy) flushLoop(ctx context.Context) {
	t := time.NewTicker(batchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.flush()
		}
	}
}

func (p *Proxy) flush() {
	p.mu.Lock()
	b := p.batch
	p.batch = nil
	p.mu.Unlock()
	if len(b) > 0 {
		p.sendAll(&api.BluetoothLERawAdvertisementsResponse{Advertisements: b})
	}
}

func (p *Proxy) sendAll(m proto.Message) {
	p.mu.Lock()
	conns := make([]*api.Conn, 0, len(p.subs))
	for c := range p.subs {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.Send(m)
	}
}
