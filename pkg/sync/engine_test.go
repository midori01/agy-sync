package sync

import (
	"testing"
)

func TestValidateELFArchitecture(t *testing.T) {
	// Fake ELF header for AMD64 (e_machine = 0x3E, 64-bit, LSB)
	amd64Header := make([]byte, 64)
	copy(amd64Header[:4], []byte{0x7F, 'E', 'L', 'F'})
	amd64Header[4] = 2 // ELFCLASS64
	amd64Header[5] = 1 // ELFDATA2LSB
	amd64Header[18] = 0x3E
	amd64Header[19] = 0x00

	// Fake ELF header for ARM64 (e_machine = 0xB7, 64-bit, LSB)
	arm64Header := make([]byte, 64)
	copy(arm64Header[:4], []byte{0x7F, 'E', 'L', 'F'})
	arm64Header[4] = 2 // ELFCLASS64
	arm64Header[5] = 1 // ELFDATA2LSB
	arm64Header[18] = 0xB7
	arm64Header[19] = 0x00

	// 1. AMD64 validation
	if !ValidateELFArchitecture(amd64Header, "amd64") {
		t.Errorf("expected amd64Header to be valid for amd64")
	}
	if !ValidateELFArchitecture(amd64Header, "x86_64") {
		t.Errorf("expected amd64Header to be valid for x86_64")
	}
	if ValidateELFArchitecture(amd64Header, "arm64") {
		t.Errorf("expected amd64Header to FAIL for arm64")
	}

	// 2. ARM64 validation
	if !ValidateELFArchitecture(arm64Header, "arm64") {
		t.Errorf("expected arm64Header to be valid for arm64")
	}
	if !ValidateELFArchitecture(arm64Header, "aarch64") {
		t.Errorf("expected arm64Header to be valid for aarch64")
	}
	if ValidateELFArchitecture(arm64Header, "amd64") {
		t.Errorf("expected arm64Header to FAIL for amd64")
	}

	// 3. Short / Invalid headers
	if ValidateELFArchitecture([]byte{0x7F, 'E', 'L', 'F'}, "amd64") {
		t.Errorf("expected short header to fail")
	}
	if ValidateELFArchitecture([]byte("not an elf binary file at all!"), "amd64") {
		t.Errorf("expected non-elf to fail")
	}

	// 4. Reject 32-bit ELF binaries
	elf32Header := make([]byte, 64)
	copy(elf32Header, amd64Header)
	elf32Header[4] = 1 // ELFCLASS32
	if ValidateELFArchitecture(elf32Header, "amd64") {
		t.Errorf("expected 32-bit ELF to FAIL validation")
	}
}
