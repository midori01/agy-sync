package cluster

import (
	"testing"

	"agy-sync/pkg/config"
)

func TestMergeRegistriesPreservesTailscaleIPAndUser(t *testing.T) {
	regA := config.Registry{
		"192.168.1.10:8022": &config.DeviceEntry{
			Alias:       "phone",
			NativeName:  "phone-native",
			UpdatedAt:   100.0,
			TailscaleIP: "",
			User:        "",
			LanIps:      []string{"192.168.1.10"},
		},
	}

	regB := config.Registry{
		"192.168.1.10:8022": &config.DeviceEntry{
			Alias:       "phone",
			NativeName:  "phone-native",
			UpdatedAt:   90.0, // Older timestamp
			TailscaleIP: "100.90.80.70",
			User:        "u0_a123",
			LanIps:      []string{"192.168.1.10"},
		},
	}

	// Case 1: regA is newer (100.0 vs 90.0), but regB has TailscaleIP and User
	merged, aChanged, bChanged := MergeRegistries(regA, regB, 0)
	entry := merged["192.168.1.10:8022"]
	if entry.TailscaleIP != "100.90.80.70" {
		t.Fatalf("expected TailscaleIP '100.90.80.70', got '%s'", entry.TailscaleIP)
	}
	if entry.User != "u0_a123" {
		t.Fatalf("expected User 'u0_a123', got '%s'", entry.User)
	}
	if !aChanged {
		t.Fatalf("expected aChanged to be true since TailscaleIP and User were merged into A")
	}
	if !bChanged {
		t.Fatalf("expected bChanged to be true since A had newer timestamp")
	}

	// Case 2: regB is newer (110.0 vs 100.0), but regA has TailscaleIP and User
	regA["192.168.1.10:8022"].TailscaleIP = "100.90.80.70"
	regA["192.168.1.10:8022"].User = "u0_a123"
	regB["192.168.1.10:8022"].UpdatedAt = 110.0
	regB["192.168.1.10:8022"].TailscaleIP = ""
	regB["192.168.1.10:8022"].User = ""

	merged2, aChanged2, bChanged2 := MergeRegistries(regA, regB, 0)
	entry2 := merged2["192.168.1.10:8022"]
	if entry2.TailscaleIP != "100.90.80.70" {
		t.Fatalf("expected TailscaleIP '100.90.80.70', got '%s'", entry2.TailscaleIP)
	}
	if entry2.User != "u0_a123" {
		t.Fatalf("expected User 'u0_a123', got '%s'", entry2.User)
	}
	if !aChanged2 {
		t.Fatalf("expected aChanged2 to be true since B had newer timestamp")
	}
	if !bChanged2 {
		t.Fatalf("expected bChanged2 to be true since TailscaleIP and User were merged into B")
	}
}
