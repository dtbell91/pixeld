package esphomeapi

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestRegistry(t *testing.T) {
	registry()
	for name, want := range map[string]uint32{"HelloRequest": 1, "PingRequest": 7, "DeviceInfoResponse": 10,
		"VoiceAssistantRequest": 90, "VoiceAssistantAudio": 106} {
		if got := idOf[protoreflectName(name)]; got != want {
			t.Errorf("%s: id %d, want %d", name, got, want)
		}
	}
	if len(byID) < 100 {
		t.Errorf("only %d message types", len(byID))
	}
}

func protoreflectName(s string) protoreflect.FullName { return protoreflect.FullName(s) }
