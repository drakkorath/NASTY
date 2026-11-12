package main

// Operations understood by the root helper (`nasty --helper`, started via pkexec).
const (
	OpCheck          = "check"           // install config, `nixos-rebuild dry-build`, then restore
	OpApply          = "apply"           // install config, `nixos-rebuild switch`, restore on failure
	OpUpdateChannels = "update_channels" // `nix-channel --update` as root
	OpRollback       = "rollback"        // `nixos-rebuild switch --rollback`
	OpCancel         = "cancel"          // interrupt the running operation
)

// Request is sent from the GUI to the helper: one JSON object per line on stdin.
type Request struct {
	Op    string       `json:"op"`
	State *SystemState `json:"state,omitempty"`
}

// Event is sent from the helper back to the GUI: one JSON object per line on stdout.
// The client also synthesises an "exit" event when the helper process ends.
type Event struct {
	Type  string `json:"type"`            // log | done | exit
	Level string `json:"level,omitempty"` // info | warn | error | cmd | ok ("" = classify the text)
	Text  string `json:"text,omitempty"`
	Op    string `json:"op,omitempty"` // for "done"
	OK    bool   `json:"ok,omitempty"` // for "done" and "exit"
}
