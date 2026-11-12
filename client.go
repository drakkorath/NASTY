package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Session is one pkexec'd helper process. One password prompt covers every
// request sent to it. Closing it (or exiting the GUI) closes the helper's stdin,
// which makes the helper cancel any running operation and exit.
type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	sendMu    sync.Mutex
	closeOnce sync.Once

	// Events delivers helper output. It is closed after the helper exits; the
	// last event before closing has Type "exit".
	Events chan Event
}

func StartSession() (*Session, error) {
	pk, err := exec.LookPath("pkexec")
	if err != nil {
		return nil, errors.New("pkexec was not found. Enable polkit (security.polkit.enable = true) so NASTY can ask for your password")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the NASTY executable: %w", err)
	}

	cmd := exec.Command(pk, self, "--helper")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start pkexec: %w", err)
	}

	s := &Session{cmd: cmd, stdin: stdin, Events: make(chan Event, 256)}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				ev = Event{Type: "log", Text: sc.Text()}
			}
			s.Events <- ev
		}
		io.Copy(io.Discard, stdout)
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			s.Events <- Event{Type: "log", Level: "warn", Text: sc.Text()}
		}
		io.Copy(io.Discard, stderr)
	}()
	go func() {
		wg.Wait()
		werr := cmd.Wait()
		ev := Event{Type: "exit", OK: werr == nil}
		if werr != nil {
			ev.Text = describeExit(werr)
		}
		s.Events <- ev
		close(s.Events)
	}()

	return s, nil
}

func describeExit(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 126:
			return "Authentication was cancelled or not authorised."
		case 127:
			return "Could not authenticate. Is a polkit authentication agent running?"
		}
		return fmt.Sprintf("The helper exited with status %d.", ee.ExitCode())
	}
	return err.Error()
}

// Send writes one request to the helper.
func (s *Session) Send(req Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_, err = s.stdin.Write(b)
	return err
}

// Close ends the session. The helper cancels any running operation and exits.
func (s *Session) Close() {
	s.closeOnce.Do(func() { _ = s.stdin.Close() })
}
