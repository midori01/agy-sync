package terminal

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type Spinner struct {
	prefix      string
	subStatus   string
	isEnabled   bool
	stopChan    chan struct{}
	doneChan    chan struct{}
	t0          time.Time
	lastLineLen int
	mu          sync.Mutex
	running     bool
}

func IsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode()&os.ModeCharDevice) != 0 && os.Getenv("TERM") != "dumb"
}

func NewSpinner(prefix string, isEnabled ...bool) *Spinner {
	enabled := true
	if len(isEnabled) > 0 {
		enabled = isEnabled[0]
	}
	enabled = enabled && IsTerminal()

	return &Spinner{
		prefix:    prefix,
		isEnabled: enabled,
		stopChan:  make(chan struct{}),
		doneChan:  make(chan struct{}),
	}
}

func (s *Spinner) SetStatus(sub string) {
	if !s.isEnabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subStatus = sub
}

func (s *Spinner) Start() {
	if !s.isEnabled {
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.t0 = time.Now()
	s.lastLineLen = 0
	s.stopChan = make(chan struct{})
	s.doneChan = make(chan struct{})
	s.mu.Unlock()

	go func() {
		defer close(s.doneChan)
		idx := 0
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				frame := SpinnerFrames[idx%len(SpinnerFrames)]
				elapsed := time.Since(s.t0).Seconds()

				s.mu.Lock()
				sub := ""
				if s.subStatus != "" {
					sub = fmt.Sprintf(" \033[90m·\033[0m %s", s.subStatus)
				}
				text := fmt.Sprintf("\r\033[1;36m%s\033[0m %s%s \033[90m(%.1fs)\033[0m", frame, s.prefix, sub, elapsed)
				w := StrWidth(text)
				padLen := 0
				if s.lastLineLen > w {
					padLen = s.lastLineLen - w
				}
				fmt.Print(text + strings.Repeat(" ", padLen))
				s.lastLineLen = w
				s.mu.Unlock()

				idx++
			}
		}
	}()
}

func (s *Spinner) Stop(finalLine ...string) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopChan)
	s.mu.Unlock()

	select {
	case <-s.doneChan:
	case <-time.After(300 * time.Millisecond):
	}

	if s.isEnabled {
		fmt.Print("\r\033[K")
		if len(finalLine) > 0 && finalLine[0] != "" {
			fmt.Println(finalLine[0])
		}
	}
}
