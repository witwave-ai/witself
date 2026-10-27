package localcell

import (
	"context"
	"errors"
)

// ServicePayload identifies one immutable, explicitly supplied host invocation.
// It contains no credentials and is never resolved through PATH.
type ServicePayload struct {
	Generation string   `json:"generation"`
	Host       string   `json:"host"`
	Args       []string `json:"args"`
}

// ServiceDescriptor is private installation-bound service intent. Revision
// changes fence subsequent launches; only explicit repair clears Faulted.
type ServiceDescriptor struct {
	Version         int            `json:"version"`
	InstallationID  string         `json:"installation_id"`
	UID             string         `json:"uid"`
	Root            string         `json:"root"`
	Adapter         string         `json:"adapter"`
	Scope           string         `json:"scope"`
	Payload         ServicePayload `json:"payload"`
	Runtime         string         `json:"runtime"`
	DesiredState    string         `json:"desired_state"`
	ResourceProfile string         `json:"resource_profile"`
	Revision        uint64         `json:"revision"`
	RepairRevision  uint64         `json:"repair_revision"`
	Faulted         bool           `json:"faulted"`
}

// ServiceIdentity is the complete registration identity, never just its label.
type ServiceIdentity = ServiceDescriptor

// ServiceSpec is the requested exact per-user registration.
type ServiceSpec = ServiceDescriptor

// ServiceObservation reports capability and exact ownership without health claims.
type ServiceObservation struct {
	Available  bool   `json:"available"`
	Registered bool   `json:"registered"`
	Owned      bool   `json:"owned"`
	Reason     string `json:"reason,omitempty"`
	Fallback   string `json:"fallback,omitempty"`
}

// ServicePlan is advisory: Apply must observe capability and ownership again.
type ServicePlan struct {
	Spec              ServiceDescriptor  `json:"spec"`
	Observation       ServiceObservation `json:"observation"`
	RequiredPrivilege string             `json:"required_privilege"`
}

// ServiceAdapter controls only an exact owned per-user registration.
type ServiceAdapter interface {
	Inspect(context.Context, ServiceIdentity) (ServiceObservation, error)
	Plan(context.Context, ServiceSpec) (ServicePlan, error)
	Apply(context.Context, ServicePlan) error
	Remove(context.Context, ServiceIdentity) error
}

var (
	// ErrServiceManagerUnavailable advertises an explicit foreground fallback.
	ErrServiceManagerUnavailable = errors.New("service_manager_unavailable: use explicit localcellharness run foreground fallback")
	// ErrServiceOwnershipConflict refuses matching labels with foreign payloads.
	ErrServiceOwnershipConflict = errors.New("service_ownership_conflict")
	// ErrServiceDesiredState prevents automatic starts against durable intent.
	ErrServiceDesiredState = errors.New("service_desired_state_refused")
)
