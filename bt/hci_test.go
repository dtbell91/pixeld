package bt

import "testing"

// Two reports in one event: each field is an array across the reports (Core spec, LE Advertising Report).
func TestReportsParsesFieldArrays(t *testing.T) {
	p := []byte{2, // num reports
		0x00, 0x04, // event types: ADV_IND, SCAN_RSP
		0x00, 0x01, // address types: public, random
		0x66, 0x55, 0x44, 0x33, 0x22, 0x11, // address 1, little-endian
		0xff, 0xee, 0xdd, 0xcc, 0xbb, 0xaa, // address 2
		2, 1, // data lengths
		0x01, 0x02, 0x03, // data
		0xc4, 0xb0, // RSSI -60, -80
	}
	var got []report
	h := &hci{adv: func(r report) { got = append(got, r) }}
	h.reports(p)
	if len(got) != 2 {
		t.Fatalf("%d reports", len(got))
	}
	if got[0].addr != 0x112233445566 || got[0].addrType != 0 || string(got[0].data) != "\x01\x02" || got[0].rssi != -60 {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].addr != 0xaabbccddeeff || got[1].evtType != 4 || string(got[1].data) != "\x03" || got[1].rssi != -80 {
		t.Fatalf("second: %+v", got[1])
	}
}
