package main

import (
	"os"
	"time"
)

// Phase is where the "review, then apply" flow currently is.
type Phase int

const (
	PhaseIdle     Phase = iota
	PhaseChecking       // dry build running (review dialog open)
	PhaseReview         // dry build finished; waiting for the user's decision
	PhaseApplying       // real rebuild running
	PhaseWorking        // update channels / rollback
)

// opState is everything about the operation currently in flight.
// Guarded by UIContext.opMu.
type opState struct {
	phase      Phase
	name       string
	started    time.Time
	checkOK    bool
	cancelling bool
	progress   Progress
	pending    SystemState // what the user asked for (becomes the baseline on success)
	sess       *Session
}

// opSnapshot is a copy the UI can read without holding the lock.
type opSnapshot struct {
	Phase      Phase
	Name       string
	Elapsed    time.Duration
	CheckOK    bool
	Cancelling bool
	Progress   string
	Gens       []Generation
	Baseline   SystemState
}

func (ctx *UIContext) snapshot() opSnapshot {
	ctx.opMu.Lock()
	defer ctx.opMu.Unlock()
	s := opSnapshot{
		Phase:      ctx.op.phase,
		Name:       ctx.op.name,
		CheckOK:    ctx.op.checkOK,
		Cancelling: ctx.op.cancelling,
		Progress:   ctx.op.progress.Summary(),
		Gens:       ctx.gens,
		Baseline:   ctx.baseline,
	}
	if ctx.op.phase != PhaseIdle {
		s.Elapsed = time.Since(ctx.op.started)
	}
	return s
}

// finishLocked returns to idle. The caller must hold opMu.
func (ctx *UIContext) finishLocked() {
	ctx.op = opState{}
	ctx.busy.Store(false)
}

// beginReview builds the diff and starts the dry-build check.
func (ctx *UIContext) beginReview() {
	if ctx.busy.Load() {
		return
	}
	state := ctx.currentState()

	ctx.opMu.Lock()
	base := ctx.baseline
	ctx.opMu.Unlock()

	ctx.reviewDiff = diffStates(base, state)

	oldText, _ := os.ReadFile(sidecarPath) // missing file == empty
	newText, err := renderSidecar(state)
	if err != nil {
		ctx.addLine(LevelError, err.Error())
		return
	}
	ctx.reviewRaw = lineDiff(splitLines(string(oldText)), splitLines(string(newText)))
	ctx.ShowRawDiff.Value = false

	for _, w := range setupWarnings() {
		ctx.addLine(LevelWarn, w)
	}
	ctx.startCheck(state)
}

func (ctx *UIContext) startCheck(state SystemState) {
	if !ctx.busy.CompareAndSwap(false, true) {
		return
	}
	sess, err := StartSession()
	if err != nil {
		ctx.busy.Store(false)
		ctx.addLine(LevelError, err.Error())
		return
	}

	ctx.opMu.Lock()
	ctx.op = opState{
		phase:   PhaseChecking,
		name:    "Checking configuration",
		started: time.Now(),
		pending: state,
		sess:    sess,
	}
	ctx.opMu.Unlock()

	ctx.addLine(LevelCmd, "==> Checking the new configuration (dry build). Nothing is changed yet.")
	go ctx.consume(sess)
	if err := sess.Send(Request{Op: OpCheck, State: &state}); err != nil {
		ctx.addLine(LevelError, "Could not talk to the helper: "+err.Error())
	}
}

// startApply runs the real rebuild on the same helper session (no second password prompt).
func (ctx *UIContext) startApply() {
	ctx.opMu.Lock()
	if ctx.op.phase != PhaseReview || !ctx.op.checkOK || ctx.op.sess == nil {
		ctx.opMu.Unlock()
		return
	}
	sess := ctx.op.sess
	state := ctx.op.pending
	ctx.op.phase = PhaseApplying
	ctx.op.name = "Applying changes"
	ctx.op.started = time.Now()
	ctx.op.progress = Progress{}
	ctx.op.cancelling = false
	ctx.opMu.Unlock()

	ctx.addLine(LevelCmd, "==> Applying changes...")
	if err := sess.Send(Request{Op: OpApply, State: &state}); err != nil {
		ctx.addLine(LevelError, "Could not talk to the helper: "+err.Error())
	}
}

// startSimple runs a single-request operation (update channels, rollback).
func (ctx *UIContext) startSimple(op, name string) {
	if !ctx.busy.CompareAndSwap(false, true) {
		return
	}
	sess, err := StartSession()
	if err != nil {
		ctx.busy.Store(false)
		ctx.addLine(LevelError, err.Error())
		return
	}

	ctx.opMu.Lock()
	ctx.op = opState{phase: PhaseWorking, name: name, started: time.Now(), sess: sess}
	ctx.opMu.Unlock()

	go ctx.consume(sess)
	if err := sess.Send(Request{Op: op}); err != nil {
		ctx.addLine(LevelError, "Could not talk to the helper: "+err.Error())
	}
	ctx.Window.Invalidate()
}

// cancelOp cancels a running operation, or closes the review dialog.
func (ctx *UIContext) cancelOp() {
	ctx.opMu.Lock()
	sess := ctx.op.sess
	switch ctx.op.phase {
	case PhaseReview:
		// Backing out of the review: end the session, nothing was changed.
		ctx.finishLocked()
		ctx.opMu.Unlock()
		if sess != nil {
			sess.Close()
		}
		ctx.addLine(LevelInfo, "Review closed. Nothing was changed.")
	case PhaseChecking, PhaseApplying, PhaseWorking:
		if ctx.op.cancelling {
			ctx.opMu.Unlock()
			return
		}
		ctx.op.cancelling = true
		ctx.opMu.Unlock()
		if sess != nil {
			_ = sess.Send(Request{Op: OpCancel})
		}
		ctx.addLine(LevelWarn, "Cancelling...")
	default:
		ctx.opMu.Unlock()
	}
	ctx.Window.Invalidate()
}

// consume forwards helper events into the UI until the helper exits.
func (ctx *UIContext) consume(sess *Session) {
	for ev := range sess.Events {
		switch ev.Type {
		case "log":
			text := cleanLine(ev.Text)
			if text == "" {
				continue
			}
			lvl, ok := parseLevel(ev.Level)
			if !ok {
				lvl = classify(text)
			}
			ctx.opMu.Lock()
			if ctx.op.sess == sess {
				ctx.op.progress.Feed(text)
			}
			ctx.opMu.Unlock()
			ctx.addLine(lvl, text)
		case "done":
			ctx.handleDone(sess, ev)
		case "exit":
			if !ev.OK && ev.Text != "" {
				ctx.addLine(LevelError, ev.Text)
			}
		}
	}
	ctx.handleSessionEnd(sess)
}

func (ctx *UIContext) handleDone(sess *Session, ev Event) {
	ctx.opMu.Lock()
	if ctx.op.sess != sess {
		ctx.opMu.Unlock()
		return
	}

	switch ev.Op {
	case OpCheck:
		ctx.op.phase = PhaseReview
		ctx.op.checkOK = ev.OK
		ctx.op.cancelling = false
		ctx.opMu.Unlock()
		if ev.OK {
			ctx.addLine(LevelOK, "Dry build passed. Nothing has been changed yet.")
		} else {
			ctx.addLine(LevelError, "The dry build failed or was cancelled. Nothing was changed.")
		}

	case OpApply:
		pending := ctx.op.pending
		ctx.finishLocked()
		if ev.OK {
			ctx.baseline = pending
		}
		ctx.opMu.Unlock()
		sess.Close()
		if ev.OK {
			ctx.addLine(LevelOK, "Done. Your changes are active.")
		} else {
			ctx.addLine(LevelError, "The apply did not complete. The previous configuration file was restored.")
		}
		ctx.reloadGenerations()

	case OpUpdateChannels:
		ctx.finishLocked()
		ctx.opMu.Unlock()
		sess.Close()
		if ev.OK {
			go SyncNixIndex(true, ctx.logf)
		}

	case OpRollback:
		ctx.finishLocked()
		ctx.opMu.Unlock()
		sess.Close()
		ctx.reloadGenerations()

	default:
		ctx.opMu.Unlock()
	}
	ctx.Window.Invalidate()
}

// handleSessionEnd runs when the helper process has exited. If we did not end
// the session ourselves (auth dismissed, crash), return to idle.
func (ctx *UIContext) handleSessionEnd(sess *Session) {
	ctx.opMu.Lock()
	if ctx.op.sess == sess {
		phase := ctx.op.phase
		ctx.finishLocked()
		ctx.opMu.Unlock()
		if phase != PhaseIdle {
			ctx.addLine(LevelWarn, "The helper stopped before the operation finished.")
		}
	} else {
		ctx.opMu.Unlock()
	}
	ctx.Window.Invalidate()
}

func (ctx *UIContext) reloadGenerations() {
	gens, _ := listGenerations()
	ctx.opMu.Lock()
	ctx.gens = gens
	ctx.opMu.Unlock()
	ctx.Window.Invalidate()
}
