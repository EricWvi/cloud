package workflow

import (
	"encoding/json"
	"time"
)

// FailureKind classifies why a node failed, with desktop's NodeFailureKind wire names.
type FailureKind string

// Failure kinds v2 can produce. Desktop kinds tied to unapplied capabilities (Skill
// materialization, baselines) are not listed because v2 never raises them.
const (
	FailureWorkflowModelNotFound     FailureKind = "workflow_model_not_found"
	FailureWorkflowRoleNotFound      FailureKind = "workflow_role_not_found"
	FailureMissingAgentRef           FailureKind = "missing_agent_ref"
	FailureMissingAgentConfig        FailureKind = "missing_agent_config"
	FailureInvalidRunPayload         FailureKind = "invalid_run_payload"
	FailurePromptTemplate            FailureKind = "prompt_template"
	FailureStructuredOutput          FailureKind = "structured_output"
	FailureSessionEndedWithoutReason FailureKind = "session_ended_without_stop_reason"
	FailureSessionBindingRejected    FailureKind = "session_binding_rejected"
	FailureRepository                FailureKind = "repository"
	FailureSession                   FailureKind = "session"
	FailureAgentRefusal              FailureKind = "agent_refusal"
	FailureUnknownStopReason         FailureKind = "unknown_stop_reason"
	FailureInterruptedByRestart      FailureKind = "interrupted_by_restart"
	FailureMultipleOutputs           FailureKind = "multiple_outputs"
	FailureConditionEvaluation       FailureKind = "condition_evaluation"
	FailureAggregatorNoMatch         FailureKind = "aggregator_no_match"
	// FailureInteractiveSession is v2's own kind for an interactive Agent node whose session
	// could not be set up. Desktop never retries an interactive node (retry_tests.rs:636-644),
	// so its failures must not read as auto-retryable; desktop has no such kind because its
	// retry policy excludes interactive attempts instead.
	FailureInteractiveSession FailureKind = "interactive_session"
	// FailureStarterUnavailable is v2's own kind for an Agent the execution bridge or the catalog
	// refused because the run's starter, whose identity its Agents execute under, is no longer an
	// active member of the workspace (removed, or the account disabled). Retrying under the same
	// identity can only fail again, so it never retries automatically; a resume stays possible
	// once the starter is back, and until then the resume preview refuses it as
	// ResumeRefusedStarterUnavailable.
	FailureStarterUnavailable FailureKind = "starter_unavailable"
)

func (k FailureKind) resumable() bool {
	switch k {
	case FailureWorkflowModelNotFound, FailureWorkflowRoleNotFound, FailureMissingAgentConfig, FailureSession, FailureSessionEndedWithoutReason,
		FailureSessionBindingRejected, FailureInterruptedByRestart, FailureRepository, FailureInteractiveSession,
		FailureStarterUnavailable:
		return true
	}
	return false
}

// InjectsIntoPrompt reports whether failures of this kind are safe and useful to feed back into
// the Agent's prompt on a subsequent retry or manual resume.
func (k FailureKind) InjectsIntoPrompt() bool {
	switch k {
	case FailureStructuredOutput, FailureAgentRefusal, FailureUnknownStopReason, FailureMultipleOutputs:
		return true
	}
	return false
}

func (k FailureKind) autoRetryable() bool {
	switch k {
	case FailureSession, FailureSessionEndedWithoutReason, FailureSessionBindingRejected,
		FailureStructuredOutput, FailureAgentRefusal, FailureUnknownStopReason:
		return true
	}
	return false
}

// Interactive returns f as the failure of an interactive Agent node: a kind that would retry
// automatically becomes FailureInteractiveSession, since a person drives the node and desktop
// never retries it. Other kinds already fail without a retry and keep their own name.
func (f Failure) Interactive() Failure {
	if f.Kind.autoRetryable() {
		f.Kind = FailureInteractiveSession
	}
	return f
}

// Failure is one node failure as reported by a runtime or the execution bridge. Output keeps
// the raw answer of a failed structured response so it stays inspectable.
type Failure struct {
	Code, PluginID, SettingID, Transport string
	Kind                                 FailureKind
	Message                              string
	SourceChain                          []string
	Output                               *string
}

// runtimeFailure classifies a swift runtime's refusal, as desktop's NodeFailure::from_runtime.
func runtimeFailure(kind, message string) Failure {
	k := FailureRepository
	switch kind {
	case "output":
		k = FailureMultipleOutputs
	case "condition":
		k = FailureConditionEvaluation
	case "aggregator":
		k = FailureAggregatorNoMatch
	}
	return Failure{Kind: k, Message: message}
}

// Detail renders the payload.error_detail object for the attempt number the store assigned.
// Flags retain desktop's meaning, including for nodes outside the automatic retry scope.
func (f Failure) Detail(attempt int, injectLastFailure bool, now time.Time) map[string]any {
	chain := f.SourceChain
	if chain == nil {
		chain = []string{}
	}
	detail := map[string]any{
		"kind":                     string(f.Kind),
		"message":                  f.Message,
		"source_chain":             chain,
		"attempt":                  attempt,
		"resumable":                f.Kind.resumable(),
		"injects_previous_failure": f.Kind.InjectsIntoPrompt() && injectLastFailure,
		"auto_retryable":           f.Kind.autoRetryable(),
		"recorded_at":              now.UnixMilli(),
	}
	for key, value := range map[string]string{"code": f.Code, "plugin_id": f.PluginID, "setting_id": f.SettingID, "transport": f.Transport} {
		if value != "" {
			detail[key] = value
		}
	}
	return detail
}

// MergePayload sets keys on a node payload object, treating NULL or invalid JSON as empty and
// keeping every other key, as desktop's merge_payload_keys.
func MergePayload(existing []byte, keys map[string]any) ([]byte, error) {
	m := map[string]any{}
	if len(existing) != 0 {
		var decoded any
		if json.Unmarshal(existing, &decoded) == nil {
			if obj, ok := decoded.(map[string]any); ok {
				m = obj
			}
		}
	}
	for k, v := range keys {
		m[k] = v
	}
	return json.Marshal(m)
}
