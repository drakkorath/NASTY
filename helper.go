package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file is the privileged half of the app. It only runs when the binary is
// started as `pkexec nasty --helper`. It never trusts the GUI: package names are
// re-validated here, the config file is rendered here, and the only file it ever
// writes is the fixed sidecar path.

const defaultNixPath = "nixpkgs=/nix/var/nix/profiles/per-user/root/channels/nixos:" +
	"nixos-config=/etc/nixos/configuration.nix:/nix/var/nix/profiles/per-user/root/channels"

type helper struct {
	enc     *json.Encoder
	outMu   sync.Mutex
	sessCtx context.Context

	opMu     sync.Mutex
	opCancel context.CancelFunc
}

// runHelper reads requests from `in` and writes events to `out`.
// When `in` reaches EOF (GUI closed or died) any running operation is cancelled.
func runHelper(in io.Reader, out io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nasty --helper must run as root (it is started through pkexec).")
		return 2
	}

	sessCtx, sessCancel := context.WithCancel(context.Background())
	defer sessCancel()

	h := &helper{enc: json.NewEncoder(out), sessCtx: sessCtx}
	reqs := make(chan Request, 8)

	go func() {
		defer close(reqs)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var req Request
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				h.log("error", "helper: bad request: "+err.Error())
				continue
			}
			if req.Op == OpCancel {
				h.cancelCurrent()
				continue
			}
			reqs <- req
		}
		sessCancel() // stdin closed: stop whatever is running and refuse queued work
	}()

	for req := range reqs {
		h.handle(req)
	}
	return 0
}

func (h *helper) send(ev Event) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_ = h.enc.Encode(ev)
}

func (h *helper) log(level, text string) {
	h.send(Event{Type: "log", Level: level, Text: text})
}

func (h *helper) cancelCurrent() {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if h.opCancel != nil {
		h.opCancel()
	}
}

func (h *helper) handle(req Request) {
	if h.sessCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(h.sessCtx)
	h.opMu.Lock()
	h.opCancel = cancel
	h.opMu.Unlock()
	defer func() {
		h.opMu.Lock()
		h.opCancel = nil
		h.opMu.Unlock()
		cancel()
	}()

	var err error
	switch req.Op {
	case OpCheck:
		err = h.doRebuild(ctx, req.State, true)
	case OpApply:
		err = h.doRebuild(ctx, req.State, false)
	case OpUpdateChannels:
		h.log("cmd", "==> Updating channels...")
		err = h.runCmd(ctx, "nix-channel", "--update")
		if err == nil {
			h.log("ok", "==> Channels updated.")
		}
	case OpRollback:
		h.log("cmd", "==> Rolling back to the previous generation...")
		err = h.runCmd(ctx, "nixos-rebuild", "switch", "--rollback")
		if err == nil {
			h.log("ok", "==> Rolled back.")
		}
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			h.log("warn", "==> Cancelled.")
		} else {
			h.log("error", "==> "+err.Error())
		}
	}
	h.send(Event{Type: "done", Op: req.Op, OK: err == nil})
}

// doRebuild installs the sidecar and runs nixos-rebuild.
// dryRun=true always restores the previous file afterwards (it is only a check).
// dryRun=false restores the previous file if the rebuild fails.
func (h *helper) doRebuild(ctx context.Context, st *SystemState, dryRun bool) error {
	if st == nil {
		return errors.New("request had no configuration")
	}
	state, dropped := sanitizeState(*st)
	for _, d := range dropped {
		h.log("warn", fmt.Sprintf("Ignoring invalid package name %q", d))
	}

	data, err := renderSidecar(state)
	if err != nil {
		return err
	}

	h.log("cmd", "==> Installing "+sidecarPath)
	restore, commit, err := installSidecar(data)
	if err != nil {
		return fmt.Errorf("could not write %s: %w", sidecarPath, err)
	}

	verb := "switch"
	if dryRun {
		verb = "dry-build"
	}
	h.log("cmd", "==> Running: nixos-rebuild "+verb)
	runErr := h.runCmd(ctx, "nixos-rebuild", verb)

	if dryRun || runErr != nil {
		if rerr := restore(); rerr != nil {
			h.log("error", "Could not restore the previous configuration: "+rerr.Error())
		} else if dryRun {
			h.log("info", "==> Dry run finished; the previous configuration file was put back.")
		} else {
			h.log("warn", "==> Rebuild failed; the previous configuration file was restored.")
		}
		return runErr
	}

	commit()
	h.log("ok", "==> System successfully updated.")
	return nil
}

// installSidecar writes the new sidecar atomically, keeping a backup on disk
// while the rebuild runs. restore() puts the previous state back; commit()
// accepts the new file and removes the backup.
func installSidecar(data []byte) (restore func() error, commit func(), err error) {
	orig, readErr := os.ReadFile(sidecarPath)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return nil, nil, readErr
	}

	bak := sidecarPath + ".bak"
	if existed {
		if werr := writeAtomic(bak, orig); werr != nil {
			return nil, nil, werr
		}
	}
	if werr := writeAtomic(sidecarPath, data); werr != nil {
		return nil, nil, werr
	}

	restore = func() error {
		var rerr error
		if existed {
			rerr = writeAtomic(sidecarPath, orig)
		} else {
			rerr = os.Remove(sidecarPath)
			if errors.Is(rerr, fs.ErrNotExist) {
				rerr = nil
			}
		}
		if rerr == nil {
			os.Remove(bak)
		}
		return rerr
	}
	commit = func() { os.Remove(bak) }
	return restore, commit, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// rootEnv restores PATH/NIX_PATH, which pkexec strips.
func rootEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "NIX_PATH=") || strings.HasPrefix(kv, "LC_ALL=") {
			continue
		}
		env = append(env, kv)
	}
	path := "/run/current-system/sw/bin:/run/wrappers/bin:/nix/var/nix/profiles/default/bin"
	if p := os.Getenv("PATH"); p != "" {
		path += ":" + p
	}
	nixPath := os.Getenv("NIX_PATH")
	if nixPath == "" {
		nixPath = defaultNixPath
	}
	return append(env, "PATH="+path, "NIX_PATH="+nixPath, "LC_ALL=C")
}

// runCmd runs a command in its own process group and streams its output.
// Cancelling ctx sends SIGINT to the group, and SIGKILL if it ignores that.
func (h *helper) runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = rootEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start %s: %w", name, err)
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.stream(stdout) }()
	go func() { defer wg.Done(); h.stream(stderr) }()
	wg.Wait()

	err = cmd.Wait()
	close(done)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// stream forwards lines to the GUI. After a scanner failure it drains the pipe
// so the child can't block on a full pipe.
func (h *helper) stream(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		h.log("", sc.Text())
	}
	if err := sc.Err(); err != nil {
		h.log("warn", "log stream error: "+err.Error())
		io.Copy(io.Discard, r)
	}
}
