package sendspin

import "math"

// timeFilter maps the light's clock to the server's: a two-dimensional Kalman filter over the offset and
// drift between the two, fed with NTP-style client/time exchanges. A port of the Sendspin time-filter
// (aiosendspin's time_sync.py, itself a port of the ESPHome one), as the spec requires.
type timeFilter struct {
	lastUpdate int64
	count      int

	offset, drift                   float64
	offsetCov, offsetDriftCov, dCov float64

	processVar, driftProcessVar, forgetVar float64

	cur timeElement
}

type timeElement struct {
	lastUpdate    int64
	offset, drift float64
	useDrift      bool
}

const (
	forgetCutoff       = 3.0 // residual (as a multiple of max_error) that triggers adaptive forgetting
	maxErrorScale      = 0.5 // max_error -> measurement standard deviation
	driftSignificance2 = 4.0 // drift is used only when drift² > this × its variance
)

func newTimeFilter() *timeFilter {
	f := &timeFilter{}
	f.processVar = 0
	f.driftProcessVar = 1e-11 * 1e-11
	f.forgetVar = 2.0 * 2.0
	f.reset()
	return f
}

func (f *timeFilter) reset() {
	f.count, f.lastUpdate = 0, 0
	f.offset, f.drift = 0, 0
	f.offsetCov, f.offsetDriftCov, f.dCov = math.Inf(1), 0, 0
	f.cur = timeElement{}
}

// update feeds one exchange: measurement = ((T2-T1)+(T3-T4))/2, maxError = ((T4-T1)-(T3-T2))/2, taken at
// local time timeAdded (all µs).
func (f *timeFilter) update(measurement, maxError, timeAdded int64) {
	if timeAdded <= f.lastUpdate {
		return
	}
	dt := float64(timeAdded - f.lastUpdate)
	f.lastUpdate = timeAdded
	sd := float64(maxError) * maxErrorScale
	mVar := sd * sd

	switch f.count {
	case 0:
		f.count++
		f.offset = float64(measurement)
		f.offsetCov = mVar
		f.drift = 0
		f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset}
		return
	case 1:
		f.count++
		f.drift = (float64(measurement) - f.offset) / dt
		f.offset = float64(measurement)
		f.dCov = (f.offsetCov + mVar) / (dt * dt)
		f.offsetCov = mVar
		f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift}
		return
	}

	offset := f.offset + f.drift*dt
	newDCov := f.dCov + dt*f.driftProcessVar
	newODCov := f.offsetDriftCov + f.dCov*dt
	newOCov := f.offsetCov + 2*f.offsetDriftCov*dt + f.dCov*dt*dt + dt*f.processVar

	residual := float64(measurement) - offset
	if f.count < 100 {
		f.count++
	} else if math.Abs(residual) > float64(maxError)*forgetCutoff {
		newDCov *= f.forgetVar
		newODCov *= f.forgetVar
		newOCov *= f.forgetVar
	}

	u := 1 / math.Max(newOCov+mVar, 1e-9)
	oGain, dGain := newOCov*u, newODCov*u
	f.offset = offset + oGain*residual
	f.drift += dGain * residual
	f.dCov = newDCov - dGain*newODCov
	f.offsetDriftCov = newODCov - dGain*newOCov
	f.offsetCov = newOCov - oGain*newOCov

	f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift,
		useDrift: f.drift*f.drift > driftSignificance2*f.dCov}
}

// serverTime converts a local time to the server's clock (µs).
func (f *timeFilter) serverTime(local int64) int64 {
	e := f.cur
	d := 0.0
	if e.useDrift {
		d = e.drift
	}
	return local + int64(math.Round(e.offset+d*float64(local-e.lastUpdate)))
}

// clientTime converts a server time to the local clock (µs).
func (f *timeFilter) clientTime(server int64) int64 {
	e := f.cur
	d := 0.0
	if e.useDrift {
		d = e.drift
	}
	return int64(math.Round((float64(server) - e.offset + d*float64(e.lastUpdate)) / (1 + d)))
}

func (f *timeFilter) synced() bool { return f.count >= 2 && !math.IsInf(f.offsetCov, 0) }

// errorUs is the offset's standard deviation estimate (µs).
func (f *timeFilter) errorUs() float64 { return math.Sqrt(f.offsetCov) }
