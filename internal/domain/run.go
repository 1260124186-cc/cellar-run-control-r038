package domain

import (
	"strings"
	"time"
)

type RunState string

const (
	RunPlanned      RunState = "planned"
	RunReserved     RunState = "reserved"
	RunFermenting   RunState = "fermenting"
	RunConditioning RunState = "conditioning"
	RunCompleted    RunState = "completed"
	RunAborted      RunState = "aborted"
)

type FermentationRun struct {
	ID             string                  `json:"id"`
	Code           string                  `json:"code"`
	FormulaID      string                  `json:"formula_id"`
	VesselID       *string                 `json:"vessel_id,omitempty"`
	PlannedVolumeL float64                 `json:"planned_volume_liters"`
	Notes          string                  `json:"notes,omitempty"`
	State          RunState                `json:"state"`
	StartedAt      *time.Time              `json:"started_at,omitempty"`
	EndedAt        *time.Time              `json:"ended_at,omitempty"`
	Observations   []GravityObservation    `json:"observations"`
	Corrections    []ObservationCorrection `json:"observation_corrections,omitempty"`
	Metrics        *RunMetrics             `json:"metrics,omitempty"`
	Version        int64                   `json:"version"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

func (r FermentationRun) Clone() FermentationRun {
	cloned := r
	cloned.Observations = append([]GravityObservation(nil), r.Observations...)
	cloned.Corrections = append([]ObservationCorrection(nil), r.Corrections...)
	if r.VesselID != nil {
		value := *r.VesselID
		cloned.VesselID = &value
	}
	if r.StartedAt != nil {
		value := *r.StartedAt
		cloned.StartedAt = &value
	}
	if r.EndedAt != nil {
		value := *r.EndedAt
		cloned.EndedAt = &value
	}
	if r.Metrics != nil {
		value := *r.Metrics
		cloned.Metrics = &value
	}
	return cloned
}

func (r FermentationRun) Validate() error {
	if err := ValidateRunCode(r.Code, "code"); err != nil {
		return err
	}
	if r.PlannedVolumeL <= 0 {
		return FieldError(CodeInvalidInput, "planned_volume_liters",
			"must be greater than zero")
	}
	if len([]rune(r.Notes)) > 1000 {
		return FieldError(CodeInvalidInput, "notes",
			"must not exceed 1000 characters")
	}
	return nil
}

func (r *FermentationRun) ReserveVessel(vesselID string, now time.Time) error {
	if r.State != RunPlanned {
		return NewError(CodeRunState, "only a planned run can reserve a vessel")
	}
	value := vesselID
	r.VesselID = &value
	r.State = RunReserved
	r.UpdatedAt = now.UTC()
	r.Version++
	return nil
}

func (r *FermentationRun) StartFermentation(now time.Time) error {
	if r.State != RunReserved || r.VesselID == nil {
		return NewError(CodeRunState, "only a reserved run can start fermentation")
	}
	stamp := now.UTC()
	r.State = RunFermenting
	r.StartedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

func (r *FermentationRun) AddObservation(observation GravityObservation) error {
	if r.State != RunFermenting || r.StartedAt == nil {
		return NewError(CodeRunState, "observations require a fermenting run")
	}
	if len(r.Observations) >= 500 {
		return NewError(CodeObservationConflict,
			"run cannot contain more than 500 observations")
	}
	var previous *GravityObservation
	if len(r.Observations) > 0 {
		value := r.Observations[len(r.Observations)-1]
		previous = &value
	}
	if err := ValidateObservation(*r.StartedAt, previous, observation, observation.ReceivedAt); err != nil {
		return err
	}
	expected := len(r.Observations) + 1
	if observation.Sequence != expected {
		return NewError(CodeObservationConflict,
			"observation sequence must be contiguous and begin at 1")
	}
	observation.RunID = r.ID
	r.Observations = append(r.Observations, observation)
	r.UpdatedAt = observation.ReceivedAt.UTC()
	r.Version++
	return nil
}

// CorrectLastObservation applies a controlled correction to the most recent
// observation. Only a run that has not entered final calculation can be
// corrected, and only its trailing observation. The original measurement is
// preserved on the run's correction history together with the reason and the
// correction time. The replacement must independently satisfy sequence, time
// ordering, gravity movement, range, and the current state's observation
// gates; otherwise the run is left untouched.
func (r *FermentationRun) CorrectLastObservation(
	sequence int,
	candidate GravityObservation,
	reason string,
	correctedAt time.Time,
) error {
	if r.State == RunCompleted || r.State == RunAborted {
		return NewError(CodeRunState,
			"completed or aborted runs cannot be corrected")
	}
	if r.State != RunFermenting && r.State != RunConditioning {
		return NewError(CodeRunState,
			"observations can only be corrected while fermenting or conditioning")
	}
	if len(r.Observations) == 0 {
		return NewError(CodeObservationConflict,
			"run has no observation to correct")
	}
	index := len(r.Observations) - 1
	target := r.Observations[index]
	if target.Sequence != sequence {
		return NewError(CodeObservationConflict,
			"only the most recent observation can be corrected")
	}
	if r.StartedAt == nil {
		return NewError(CodeRunState, "run has no recorded start")
	}
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return FieldError(CodeInvalidInput, "reason",
			"a correction reason is required")
	}
	if len([]rune(trimmed)) > 500 {
		return FieldError(CodeInvalidInput, "reason",
			"must not exceed 500 characters")
	}
	if candidate.Sequence != sequence {
		return NewError(CodeObservationConflict,
			"a correction must keep the original observation sequence")
	}

	candidate.RunID = r.ID
	stamp := correctedAt.UTC()
	candidate.ReceivedAt = stamp
	var previous *GravityObservation
	if index > 0 {
		value := r.Observations[index-1]
		previous = &value
	}
	if err := ValidateObservation(*r.StartedAt, previous, candidate, stamp); err != nil {
		return err
	}

	candidate.ObservedAt = candidate.ObservedAt.UTC()
	r.Observations[index] = candidate
	if err := r.validateObservationGates(); err != nil {
		// Defensive: restore the original before reporting failure so the
		// aggregate cannot escape in an invalid state.
		r.Observations[index] = target
		return err
	}

	r.Corrections = append(r.Corrections, ObservationCorrection{
		Sequence:    sequence,
		Before:      target,
		After:       candidate,
		Reason:      trimmed,
		CorrectedAt: stamp,
	})
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

// validateObservationGates re-establishes the observation-count threshold for
// the state the run currently holds. A correction never changes the count, so
// the threshold cannot stop being met; it is rechecked so a correction can
// never strand the run below its required observation count.
func (r *FermentationRun) validateObservationGates() error {
	if r.State == RunConditioning && len(r.Observations) < 2 {
		return NewError(CodeRunState,
			"conditioning requires at least two observations")
	}
	return nil
}

func (r *FermentationRun) BeginConditioning(now time.Time) error {
	if r.State != RunFermenting {
		return NewError(CodeRunState, "only a fermenting run can enter conditioning")
	}
	if len(r.Observations) < 2 {
		return NewError(CodeRunState,
			"conditioning requires at least two observations")
	}
	r.State = RunConditioning
	r.UpdatedAt = now.UTC()
	r.Version++
	return nil
}

func (r *FermentationRun) Complete(originalGravity float64, now time.Time) error {
	if r.State != RunConditioning {
		return NewError(CodeRunState, "only a conditioning run can be completed")
	}
	if len(r.Observations) < 3 {
		return NewError(CodeRunState,
			"completion requires at least three observations")
	}
	stamp := now.UTC()
	final := r.Observations[len(r.Observations)-1]
	metrics := CalculateMetrics(originalGravity, final.Gravity, len(r.Observations))
	r.Metrics = &metrics
	r.State = RunCompleted
	r.EndedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

type PreparedRunTransition struct {
	Run FermentationRun
}

func (r FermentationRun) PrepareObservation(
	candidate GravityObservation,
) (PreparedRunTransition, error) {
	planned := r.Clone()
	if err := planned.AddObservation(candidate); err != nil {
		return PreparedRunTransition{}, err
	}
	return PreparedRunTransition{Run: planned.Clone()}, nil
}

func (r FermentationRun) PrepareCompletion(
	originalGravity float64,
) (PreparedRunTransition, error) {
	planned := r.Clone()
	if err := planned.Complete(originalGravity, planned.UpdatedAt); err != nil {
		return PreparedRunTransition{}, err
	}
	return PreparedRunTransition{Run: planned.Clone()}, nil
}

func (p PreparedRunTransition) WithCommitTime(now time.Time) PreparedRunTransition {
	stamp := now.UTC()
	p.Run.UpdatedAt = stamp
	if p.Run.State == RunCompleted {
		ended := stamp
		p.Run.EndedAt = &ended
	}
	return p
}

func (p PreparedRunTransition) Apply(snapshot *Snapshot, id string) error {
	if p.Run.ID == "" || p.Run.ID != id {
		return NewError(CodeRunState, "prepared transition does not match the target run")
	}
	snapshot.Runs[id] = p.Run.Clone()
	return nil
}

func (r *FermentationRun) Abort(now time.Time) error {
	if r.State == RunCompleted || r.State == RunAborted {
		return NewError(CodeRunState, "completed or aborted runs cannot be aborted")
	}
	stamp := now.UTC()
	r.State = RunAborted
	r.EndedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

func (r FermentationRun) HadStarted() bool {
	return r.StartedAt != nil
}
