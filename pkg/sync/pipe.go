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

	consumerErr := consumer.Wait()
	_ = pipe.Close()
	if consumerErr != nil && producer.Process != nil {
		_ = producer.Process.Kill()
	}
	producerErr := producer.Wait()

	if consumerErr != nil && producerErr != nil {
		return fmt.Errorf("consumer: %v; producer: %v", consumerErr, producerErr)
	}
	if consumerErr != nil {
		return consumerErr
	}
	return producerErr
}
