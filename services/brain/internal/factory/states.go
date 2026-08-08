package factory

import (
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// runStateText maps the bounded run lifecycle vocabulary to its canonical
// storage form; zero values are invalid at the kernel boundary.
func runStateText(state contractsv1.ChangeRunState) (string, error) {
	switch state {
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING:
		return "PLANNING", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY:
		return "READY", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING:
		return "RUNNING", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW:
		return "REVIEW", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY:
		return "CANDIDATE_READY", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED:
		return "COMPLETED", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED:
		return "FAILED", nil
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED:
		return "CANCELLED", nil
	}
	return "", fmt.Errorf("%w: run state %d", ErrInvalidInput, int32(state))
}

func runStateFromText(value string) (contractsv1.ChangeRunState, error) {
	switch value {
	case "PLANNING":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING, nil
	case "READY":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY, nil
	case "RUNNING":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING, nil
	case "REVIEW":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW, nil
	case "CANDIDATE_READY":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY, nil
	case "COMPLETED":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED, nil
	case "FAILED":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED, nil
	case "CANCELLED":
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED, nil
	}
	return contractsv1.ChangeRunState_CHANGE_RUN_STATE_UNSPECIFIED, fmt.Errorf("%w: run state %q", ErrInvalidInput, value)
}

func terminalRunState(state contractsv1.ChangeRunState) bool {
	return state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED
}

// validRunTransition mirrors the bounded run lifecycle: planning → ready →
// running → review → candidate-ready → completed, with failure reachable from
// every non-terminal state and cancellation recorded only through
// CancelChangeRun.
func validRunTransition(from, to contractsv1.ChangeRunState) bool {
	if terminalRunState(from) {
		return false
	}
	switch to {
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY:
		return from == contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING:
		return from == contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW:
		return from == contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY:
		return from == contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED:
		return from == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED:
		return from != contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED
	case contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED:
		return true
	}
	return false
}

func gateKindText(kind contractsv1.FactoryGateKind) (string, error) {
	switch kind {
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD:
		return "BUILD", nil
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST:
		return "TEST", nil
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS:
		return "DOCS", nil
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY:
		return "SECURITY", nil
	}
	return "", fmt.Errorf("%w: gate kind %d", ErrInvalidInput, int32(kind))
}

func gateKindFromText(value string) (contractsv1.FactoryGateKind, error) {
	switch value {
	case "BUILD":
		return contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, nil
	case "TEST":
		return contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, nil
	case "DOCS":
		return contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS, nil
	case "SECURITY":
		return contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY, nil
	}
	return contractsv1.FactoryGateKind_FACTORY_GATE_KIND_UNSPECIFIED, fmt.Errorf("%w: gate kind %q", ErrInvalidInput, value)
}

func gateStatusText(status contractsv1.FactoryGateStatus) (string, error) {
	switch status {
	case contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING:
		return "PENDING", nil
	case contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING:
		return "RUNNING", nil
	case contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED:
		return "PASSED", nil
	case contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED:
		return "FAILED", nil
	}
	return "", fmt.Errorf("%w: gate status %d", ErrInvalidInput, int32(status))
}

func gateStatusFromText(value string) (contractsv1.FactoryGateStatus, error) {
	switch value {
	case "PENDING":
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING, nil
	case "RUNNING":
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING, nil
	case "PASSED":
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED, nil
	case "FAILED":
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED, nil
	}
	return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_UNSPECIFIED, fmt.Errorf("%w: gate status %q", ErrInvalidInput, value)
}

func candidateStateText(state contractsv1.CandidateState) (string, error) {
	switch state {
	case contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED:
		return "PROPOSED", nil
	case contractsv1.CandidateState_CANDIDATE_STATE_APPLIED:
		return "APPLIED", nil
	case contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED:
		return "VERIFIED", nil
	case contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED:
		return "REVIEWED", nil
	case contractsv1.CandidateState_CANDIDATE_STATE_RETAINED:
		return "RETAINED", nil
	case contractsv1.CandidateState_CANDIDATE_STATE_REJECTED:
		return "REJECTED", nil
	}
	return "", fmt.Errorf("%w: candidate state %d", ErrInvalidInput, int32(state))
}

func candidateStateFromText(value string) (contractsv1.CandidateState, error) {
	switch value {
	case "PROPOSED":
		return contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED, nil
	case "APPLIED":
		return contractsv1.CandidateState_CANDIDATE_STATE_APPLIED, nil
	case "VERIFIED":
		return contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil
	case "REVIEWED":
		return contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED, nil
	case "RETAINED":
		return contractsv1.CandidateState_CANDIDATE_STATE_RETAINED, nil
	case "REJECTED":
		return contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, nil
	}
	return contractsv1.CandidateState_CANDIDATE_STATE_UNSPECIFIED, fmt.Errorf("%w: candidate state %q", ErrInvalidInput, value)
}

// validCandidateTransition mirrors the bounded all-or-nothing candidate
// lifecycle; rejection is reachable from every non-terminal state.
func validCandidateTransition(from, to contractsv1.CandidateState) bool {
	if from == contractsv1.CandidateState_CANDIDATE_STATE_RETAINED ||
		from == contractsv1.CandidateState_CANDIDATE_STATE_REJECTED {
		return false
	}
	switch to {
	case contractsv1.CandidateState_CANDIDATE_STATE_APPLIED:
		return from == contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED
	case contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED:
		return from == contractsv1.CandidateState_CANDIDATE_STATE_APPLIED
	case contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED:
		return from == contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED
	case contractsv1.CandidateState_CANDIDATE_STATE_RETAINED:
		return from == contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED
	case contractsv1.CandidateState_CANDIDATE_STATE_REJECTED:
		return true
	}
	return false
}

func terminalCandidateState(state contractsv1.CandidateState) bool {
	return state == contractsv1.CandidateState_CANDIDATE_STATE_RETAINED ||
		state == contractsv1.CandidateState_CANDIDATE_STATE_REJECTED
}
