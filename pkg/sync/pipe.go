package sync

import (
	"fmt"
	"os/exec"
)

// RunPipedCommands executes producer and consumer connected via an OS pipe safely,
// ensuring both processes terminate properly without pipe deadlocks or zombie leaks.
func RunPipedCommands(producer *exec.Cmd, consumer *exec.Cmd) error {
	pipe, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	consumer.Stdin = pipe

	if err := producer.Start(); err != nil {
		_ = pipe.Close()
		return err
	}
	if err := consumer.Start(); err != nil {
		if producer.Process != nil {
			_ = producer.Process.Kill()
		}
		_ = pipe.Close()
		_ = producer.Wait()
		return err
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
			if err != nil {
				// Consumer failed: terminate producer to prevent hanging on writing to full pipe
				_ = pipe.Close()
				if producer.Process != nil {
					_ = producer.Process.Kill()
				}
			}
		}
	}
	_ = pipe.Close()

	if consumerErr != nil && producerErr != nil {
		return fmt.Errorf("consumer: %v; producer: %v", consumerErr, producerErr)
	}
	if consumerErr != nil {
		return consumerErr
	}
	return producerErr
}
