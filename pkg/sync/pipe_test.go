package sync

import (
	"bytes"
	"crypto/rand"
	"io"
	"os/exec"
	"testing"
)

func TestRunPipedCommandsWithBytesStress(t *testing.T) {
	// Generate 2MB of test data
	dataSize := 2 * 1024 * 1024
	testData := make([]byte, dataSize)
	_, _ = rand.Read(testData)

	producer := exec.Command("cat")
	producer.Stdin = bytes.NewReader(testData)

	var outputBuf bytes.Buffer
	consumer := exec.Command("cat")
	consumer.Stdout = &outputBuf

	var progressCalls int
	nBytes, err := RunPipedCommandsWithBytes(producer, consumer, func(cur int64) {
		progressCalls++
	})
	if err != nil {
		t.Fatalf("RunPipedCommandsWithBytes failed: %v", err)
	}

	if nBytes != int64(dataSize) {
		t.Fatalf("expected %d bytes transferred, got %d", dataSize, nBytes)
	}

	if outputBuf.Len() != dataSize {
		t.Fatalf("expected output buffer size %d, got %d", dataSize, outputBuf.Len())
	}

	if !bytes.Equal(outputBuf.Bytes(), testData) {
		t.Fatal("transferred data does not match original data")
	}
}

func TestRunPipedCommandsProducerError(t *testing.T) {
	// Producer fails with non-zero exit
	producer := exec.Command("sh", "-c", "echo 'producer error' >&2; exit 2")
	consumer := exec.Command("cat")
	consumer.Stdout = io.Discard

	_, err := RunPipedCommandsWithBytes(producer, consumer)
	if err == nil {
		t.Fatal("expected error from failing producer, got nil")
	}
}

func TestRunPipedCommandsConsumerError(t *testing.T) {
	// Consumer fails with non-zero exit
	producer := exec.Command("cat")
	producer.Stdin = bytes.NewReader([]byte("test data"))

	consumer := exec.Command("sh", "-c", "echo 'consumer error' >&2; exit 3")
	_, err := RunPipedCommandsWithBytes(producer, consumer)
	if err == nil {
		t.Fatal("expected error from failing consumer, got nil")
	}
}
