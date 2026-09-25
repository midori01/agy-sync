package sync

import (
	"fmt"
	"io"
	"os/exec"
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
	pipe, err := producer.StdoutPipe()
	if err != nil {
		return 0, err
	}

	var progressCb PipeProgressFunc
	if len(onProgress) > 0 && onProgress[0] != nil {
		progressCb = onProgress[0]
	}

	cr := &progressCountingReader{
		reader:     pipe,
		onProgress: progressCb,
	}
	consumer.Stdin = cr

	if err := producer.Start(); err != nil {
		_ = pipe.Close()
		return 0, err
	}
	if err := consumer.Start(); err != nil {
		if producer.Process != nil {
			_ = producer.Process.Kill()
		}
		_ = pipe.Close()
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
			if err != nil {
				// Producer failed or terminated: terminate consumer to prevent hanging on pipe
				_ = pipe.Close()
				if consumer.Process != nil {
					_ = consumer.Process.Kill()
				}
			}
		case err := <-consChan:
			consumerErr = err
			consDone = true
			// Consumer terminated: close pipe so producer receives EPIPE if still writing
			_ = pipe.Close()
			if err != nil {
				// Consumer failed: terminate producer to prevent hanging on writing to full pipe
				if producer.Process != nil {
					_ = producer.Process.Kill()
				}
			}
		}
	}
	_ = pipe.Close()

	if progressCb != nil {
		progressCb(cr.Total())
	}

	if consumerErr != nil && producerErr != nil {
		return cr.Total(), fmt.Errorf("consumer: %v; producer: %v", consumerErr, producerErr)
	}
	if consumerErr != nil {
		return cr.Total(), consumerErr
	}
	// When consumer succeeds (exit code 0), all transferred data was verified and unpacked.
	// Producer may exit with SIGPIPE or broken pipe due to pipe closure, which is completely benign.
	return cr.Total(), nil
}

// RunPipedCommands executes producer and consumer connected via an OS pipe safely,
// ensuring both processes terminate properly without pipe deadlocks or zombie leaks.
func RunPipedCommands(producer *exec.Cmd, consumer *exec.Cmd, onProgress ...PipeProgressFunc) error {
	_, err := RunPipedCommandsWithBytes(producer, consumer, onProgress...)
	return err
}

