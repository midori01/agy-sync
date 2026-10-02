package sync

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

type PipeProgressFunc func(transferred int64)

type progressCountingReader struct {
	reader     io.Reader
	onProgress PipeProgressFunc
	total      atomic.Int64
	lastUpdate time.Time
}

func (cr *progressCountingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	if n > 0 {
		cur := cr.total.Add(int64(n))
		now := time.Now()
		if cr.onProgress != nil && (cr.lastUpdate.IsZero() || now.Sub(cr.lastUpdate) >= 100*time.Millisecond || err != nil) {
			cr.lastUpdate = now
			cr.onProgress(cur)
		}
	}
	return n, err
}

func (cr *progressCountingReader) Total() int64 {
	return cr.total.Load()
}

// RunPipedCommandsWithBytes executes producer and consumer connected via an OS pipe safely,
// tracking total bytes transferred and streaming real-time progress callbacks.
func RunPipedCommandsWithBytes(producer *exec.Cmd, consumer *exec.Cmd, onProgress ...PipeProgressFunc) (int64, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}

	var progressCb PipeProgressFunc
	if len(onProgress) > 0 && onProgress[0] != nil {
		progressCb = onProgress[0]
	}

	cr := &progressCountingReader{
		reader:     r,
		onProgress: progressCb,
	}
	producer.Stdout = w
	consumer.Stdin = cr

	var prodErrBuf, consErrBuf bytes.Buffer
	if producer.Stderr == nil {
		producer.Stderr = &prodErrBuf
	}
	if consumer.Stderr == nil {
		consumer.Stderr = &consErrBuf
	}

	if err := producer.Start(); err != nil {
		_ = w.Close()
		_ = r.Close()
		return 0, err
	}
	// Close write end in parent process so only the producer holds the write descriptor.
	// When producer exits, the OS closes producer's fd, allowing reader to read until clean EOF.
	_ = w.Close()

	if err := consumer.Start(); err != nil {
		if producer.Process != nil {
			_ = producer.Process.Kill()
		}
		_ = r.Close()
		_ = producer.Wait()
		return 0, err
	}

	prodChan := make(chan error, 1)
	consChan := make(chan error, 1)

	go func() {
		prodChan <- producer.Wait()
	}()
	go func() {
		consChan <- consumer.Wait()
	}()

	var producerErr, consumerErr error
	prodDone := false
	consDone := false

	for !prodDone || !consDone {
		select {
		case err := <-prodChan:
			producerErr = err
			prodDone = true
			// When producer finishes, its write fd is closed.
			// Consumer will naturally read remaining bytes from pipe buffer until EOF.
			// Do NOT kill consumer or close r prematurely.
		case err := <-consChan:
			consumerErr = err
			consDone = true
			// Consumer finished. Close r so producer receives EPIPE if still writing.
			_ = r.Close()
			if err != nil && !prodDone {
				// Consumer failed early: terminate producer to avoid leaking resources.
				if producer.Process != nil {
					_ = producer.Process.Kill()
				}
			}
		}
	}
	_ = r.Close()

	if progressCb != nil {
		progressCb(cr.Total())
	}

	if consumerErr != nil && producerErr != nil {
		return cr.Total(), fmt.Errorf("consumer: %v (%s); producer: %v (%s)",
			consumerErr, strings.TrimSpace(consErrBuf.String()),
			producerErr, strings.TrimSpace(prodErrBuf.String()))
	}
	if consumerErr != nil {
		msg := strings.TrimSpace(consErrBuf.String())
		if msg != "" {
			return cr.Total(), fmt.Errorf("%v: %s", consumerErr, msg)
		}
		return cr.Total(), consumerErr
	}
	if producerErr != nil {
		msg := strings.TrimSpace(prodErrBuf.String())
		if msg != "" {
			return cr.Total(), fmt.Errorf("producer: %v (%s)", producerErr, msg)
		}
		return cr.Total(), producerErr
	}

	return cr.Total(), nil
}

// RunPipedCommands executes producer and consumer connected via an OS pipe safely,
// ensuring both processes terminate properly without pipe deadlocks or zombie leaks.
func RunPipedCommands(producer *exec.Cmd, consumer *exec.Cmd, onProgress ...PipeProgressFunc) error {
	_, err := RunPipedCommandsWithBytes(producer, consumer, onProgress...)
	return err
}

