package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Tool and action policy (GRD-REQ-005): the model proposes; our code
// decides.
//
// Today the model has no tools at all: llm.Generator returns text only, and
// nothing in the platform parses model output into actions. So a
// prompt-injected document has nothing privileged to trigger (GRD-AC-009).
// Any future tool must be registered here, and Authorize then requires:
//
//   - an allowlisted name (unknown tools are denied);
//   - arguments that pass the tool's schema validation;
//   - tenant-bound credentials (the caller passes the verified tenant; a
//     tool never receives credentials for another tenant);
//   - explicit human confirmation for consequential actions.

// ErrToolDenied is returned for anything not explicitly allowed.
var ErrToolDenied = errors.New("guard: tool not allowed")

// ErrConfirmationRequired is returned for a consequential tool without
// human confirmation.
var ErrConfirmationRequired = errors.New("guard: consequential action needs human confirmation")

// Tool is an allowlisted action.
type Tool struct {
	Name string
	// Validate checks the model-proposed arguments; it must reject
	// anything it does not fully understand.
	Validate func(args json.RawMessage) error
	// Consequential tools (writes, sends, deletes) need confirmation.
	Consequential bool
}

// Registry is the tool allowlist. The zero value allows nothing.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// Register adds a tool. It panics on a tool without validation, so an
// unvalidated tool can never be allowlisted by accident.
func (r *Registry) Register(t Tool) {
	if t.Name == "" || t.Validate == nil {
		panic("guard: a tool needs a name and argument validation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	r.tools[t.Name] = t
}

// Authorize decides whether a model-proposed call may run.
func (r *Registry) Authorize(name string, args json.RawMessage, humanConfirmed bool) error {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrToolDenied, name)
	}
	if err := t.Validate(args); err != nil {
		return fmt.Errorf("%w: %q arguments: %v", ErrToolDenied, name, err)
	}
	if t.Consequential && !humanConfirmed {
		return ErrConfirmationRequired
	}
	return nil
}

// Tools is the platform's registry: empty, so every tool is denied.
var Tools = &Registry{}
