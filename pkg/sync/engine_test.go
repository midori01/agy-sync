package sync

import (
	"os"
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

func TestValidateBinaryArchitecture(t *testing.T) {
	// Fake Darwin Mach-O 64-bit AMD64 (MH_MAGIC_64 = 0xCF, 0xFA, 0xED, 0xFE, cputype = 0x01000007)
	darwinAmd64 := make([]byte, 64)
	copy(darwinAmd64[:4], []byte{0xCF, 0xFA, 0xED, 0xFE})
	darwinAmd64[4] = 0x07
	darwinAmd64[5] = 0x00
	darwinAmd64[6] = 0x00
	darwinAmd64[7] = 0x01

	// Fake Darwin Mach-O 64-bit ARM64 (MH_MAGIC_64 = 0xCF, 0xFA, 0xED, 0xFE, cputype = 0x0100000C)
	darwinArm64 := make([]byte, 64)
	copy(darwinArm64[:4], []byte{0xCF, 0xFA, 0xED, 0xFE})
	darwinArm64[4] = 0x0C
	darwinArm64[5] = 0x00
	darwinArm64[6] = 0x00
	darwinArm64[7] = 0x01

	// Fake Darwin Universal / Fat binary (0xCA, 0xFE, 0xBA, 0xBE)
	darwinFat := make([]byte, 64)
	copy(darwinFat[:4], []byte{0xCA, 0xFE, 0xBA, 0xBE})

	// 1. Darwin AMD64
	if !ValidateBinaryArchitecture(darwinAmd64, "darwin", "amd64") {
		t.Errorf("expected darwinAmd64 to be valid for darwin/amd64")
	}
	if !ValidateBinaryArchitecture(darwinAmd64, "macos", "x86_64") {
		t.Errorf("expected darwinAmd64 to be valid for macos/x86_64")
	}
	if ValidateBinaryArchitecture(darwinAmd64, "darwin", "arm64") {
		t.Errorf("expected darwinAmd64 to FAIL for darwin/arm64")
	}
	if ValidateBinaryArchitecture(darwinAmd64, "linux", "amd64") {
		t.Errorf("expected darwinAmd64 to FAIL for linux/amd64")
	}

	// 2. Darwin ARM64
	if !ValidateBinaryArchitecture(darwinArm64, "darwin", "arm64") {
		t.Errorf("expected darwinArm64 to be valid for darwin/arm64")
	}
	if !ValidateBinaryArchitecture(darwinArm64, "macos", "aarch64") {
		t.Errorf("expected darwinArm64 to be valid for macos/aarch64")
	}
	if ValidateBinaryArchitecture(darwinArm64, "darwin", "amd64") {
		t.Errorf("expected darwinArm64 to FAIL for darwin/amd64")
	}
	if ValidateBinaryArchitecture(darwinArm64, "linux", "arm64") {
		t.Errorf("expected darwinArm64 to FAIL for linux/arm64")
	}

	// 3. Darwin Fat/Universal binary
	if !ValidateBinaryArchitecture(darwinFat, "darwin", "amd64") {
		t.Errorf("expected darwinFat to be valid for darwin/amd64")
	}
	if !ValidateBinaryArchitecture(darwinFat, "darwin", "arm64") {
		t.Errorf("expected darwinFat to be valid for darwin/arm64")
	}
	if ValidateBinaryArchitecture(darwinFat, "linux", "amd64") {
		t.Errorf("expected darwinFat to FAIL for linux/amd64")
	}
}

func TestGetSessionDirSizeFilter(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Artifacts & valid logs (should be counted)
	_ = os.WriteFile(tmpDir+"/artifact.md", []byte("hello world 123"), 0644) // 15 bytes
	_ = os.MkdirAll(tmpDir+"/.system_generated/logs", 0755)
	_ = os.WriteFile(tmpDir+"/.system_generated/logs/transcript.jsonl", []byte("{\"step\":1}"), 0644) // 10 bytes

	// 2. Tasks & steps logs (must be SKIPPED)
	_ = os.MkdirAll(tmpDir+"/.system_generated/tasks", 0755)
	_ = os.WriteFile(tmpDir+"/.system_generated/tasks/task-1.log", make([]byte, 10000), 0644)
	_ = os.MkdirAll(tmpDir+"/.system_generated/steps", 0755)
	_ = os.WriteFile(tmpDir+"/.system_generated/steps/step-1.txt", make([]byte, 10000), 0644)

	// 3. Scratch subdirectories (e.g. ref_repo, cmp_gcc, decompiled) - must be SKIPPED
	_ = os.MkdirAll(tmpDir+"/scratch/ref_repo/.git", 0755)
	_ = os.WriteFile(tmpDir+"/scratch/ref_repo/kernel.c", make([]byte, 50000), 0644)

	// 4. Scratch heavy binaries & images - must be SKIPPED
	_ = os.WriteFile(tmpDir+"/scratch/boot.img", make([]byte, 20000), 0644)
	_ = os.WriteFile(tmpDir+"/scratch/app.apk", make([]byte, 20000), 0644)

	// 5. Small valid scratch script <= 512KB (should be counted)
	_ = os.WriteFile(tmpDir+"/scratch/test.py", []byte("print('ok')\n"), 0644) // 12 bytes

	sz := getSessionDirSize(tmpDir)
	expected := int64(15 + 10 + 12) // 37 bytes
	if sz != expected {
		t.Errorf("getSessionDirSize expected %d bytes, got %d", expected, sz)
	}
}

