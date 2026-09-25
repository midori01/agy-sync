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
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.t0 = time.Now()
	s.lastLineLen = 0
	if !s.isEnabled {
		s.mu.Unlock()
		return
	}
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

				termW := GetTerminalWidth()
				maxW := termW - 1
				if maxW < 20 {
					maxW = 20
				}

				s.mu.Lock()
				framePart := fmt.Sprintf("\033[1;36m%s\033[0m ", frame)
				timePart := fmt.Sprintf(" \033[90m(%.1fs)\033[0m", elapsed)
				budget := maxW - StrWidth(framePart) - StrWidth(timePart)
				if budget < 8 {
					budget = 8
				}

				mainText := s.prefix
				if s.subStatus != "" {
					mainText = fmt.Sprintf("%s · %s", s.prefix, s.subStatus)
				}
				if StrWidth(mainText) > budget {
					mainText = TruncateWidth(mainText, budget)
				}

				text := fmt.Sprintf("\r%s%s%s", framePart, mainText, timePart)
				w := StrWidth(text)
				padLen := 0
				if s.lastLineLen > w {
					padLen = s.lastLineLen - w
				}
				if w+padLen > maxW {
					padLen = maxW - w
					if padLen < 0 {
						padLen = 0
					}
				}
				fmt.Print(text + strings.Repeat(" ", padLen))
				s.lastLineLen = w + padLen
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
		if len(finalLine) > 0 && finalLine[0] != "" {
			cleanLine := ansiRegex.ReplaceAllString(finalLine[0], "")
			fmt.Println(cleanLine)
		}
		return
	}
	s.running = false
	if s.isEnabled {
		close(s.stopChan)
	}
	s.mu.Unlock()

	if s.isEnabled {
		select {
		case <-s.doneChan:
		case <-time.After(300 * time.Millisecond):
		}
		fmt.Print("\r\033[K")
		if len(finalLine) > 0 && finalLine[0] != "" {
			fmt.Println(finalLine[0])
		}
	} else {
		if len(finalLine) > 0 && finalLine[0] != "" {
			cleanLine := ansiRegex.ReplaceAllString(finalLine[0], "")
			fmt.Println(cleanLine)
		}
	}
}
