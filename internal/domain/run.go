package domain

import "time"

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
	ID             string               `json:"id"`
	Code           string               `json:"code"`
	FormulaID      string               `json:"formula_id"`
	VesselID       *string              `json:"vessel_id,omitempty"`
	PlannedVolumeL float64              `json:"planned_volume_liters"`
	Notes          string               `json:"notes,omitempty"`
	State          RunState             `json:"state"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	EndedAt        *time.Time           `json:"ended_at,omitempty"`
	Observations   []GravityObservation `json:"observations"`
	Metrics        *RunMetrics          `json:"metrics,omitempty"`
	Version        int64                `json:"version"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

func (r FermentationRun) Clone() FermentationRun {
	cloned := r
	cloned.Observations = make([]GravityObservation, 0, len(r.Observations))
	for _, observation := range r.Observations {
		copied := observation
		if len(observation.Corrections) > 0 {
			copied.Corrections = append(
				[]ObservationCorrection(nil), observation.Corrections...)
		}
		cloned.Observations = append(cloned.Observations, copied)
	}
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

// CorrectObservation applies a controlled correction to the most recent
// observation. Only a run that has not entered final calculation (fermenting
// or conditioning) can be corrected, and only its tail observation can change.
// The replaced value, the reason, and the correction time are appended to that
// observation's correction history; the original receipt time is retained.
// Sequence contiguity, strict time order, and gravity movement are rechecked
// against the preceding observation so the corrected series stays valid.
func (r *FermentationRun) CorrectObservation(
	sequence int,
	candidate GravityObservation,
	reason string,
	correctedAt time.Time,
) error {
	if r.State == RunCompleted || r.State == RunAborted {
		return NewError(CodeRunState,
			"completed or aborted runs are immutable")
	}
	if r.State != RunFermenting && r.State != RunConditioning {
		return NewError(CodeRunState,
			"observations can only be corrected on a fermenting or conditioning run")
	}
	if len(r.Observations) == 0 {
		return NewError(CodeObservationConflict,
			"run has no observation to correct")
	}
	if err := ValidateCorrectionReason(reason); err != nil {
		return err
	}
	tail := r.Observations[len(r.Observations)-1]
	if sequence != tail.Sequence {
		return NewError(CodeObservationConflict,
			"only the most recent observation can be corrected")
	}

	candidate.Sequence = tail.Sequence
	candidate.RunID = r.ID
	var previous *GravityObservation
	if len(r.Observations) > 1 {
		value := r.Observations[len(r.Observations)-2]
		previous = &value
	}
	if err := ValidateObservation(*r.StartedAt, previous, candidate, correctedAt); err != nil {
		return err
	}

	stamp := correctedAt.UTC()
	correction := ObservationCorrection{
		PreviousGravity:      tail.Gravity,
		PreviousTemperatureC: tail.TemperatureC,
		PreviousObservedAt:   tail.ObservedAt.UTC(),
		PreviousNote:         tail.Note,
		Reason:               reason,
		CorrectedAt:          stamp,
	}
	candidate.ReceivedAt = tail.ReceivedAt.UTC()
	candidate.Corrections = append(append([]ObservationCorrection(nil),
		tail.Corrections...), correction)

	updated := append([]GravityObservation(nil), r.Observations...)
	updated[len(updated)-1] = candidate
	if err := ValidateObservationSeries(r.State, *r.StartedAt, updated, stamp); err != nil {
		return err
	}
	r.Observations = updated
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

// ValidateObservationSeries re-establishes every invariant a committed series
// must satisfy: contiguous one-based sequences, values within range, observed
// times that strictly increase after the run start, gravity that never rises
// more than 0.010 above the preceding value, and the observation-count gate of
// the run's current lifecycle state. tailCommitAt is used as the "current"
// reference when checking the tail observation's observed time, since a
// correction is committed at correction time even though it keeps the
// original receipt time.
func ValidateObservationSeries(
	state RunState,
	runStartedAt time.Time,
	observations []GravityObservation,
	tailCommitAt time.Time,
) error {
	var previous *GravityObservation
	for index := range observations {
		observation := observations[index]
		if observation.Sequence != index+1 {
			return NewError(CodeObservationConflict,
				"observation sequence must be contiguous and begin at 1")
		}
		now := observation.ReceivedAt
		if index == len(observations)-1 && !tailCommitAt.IsZero() {
			now = tailCommitAt
		}
		if err := ValidateObservation(runStartedAt, previous, observation, now); err != nil {
			return err
		}
		previous = &observations[index]
	}
	switch state {
	case RunConditioning, RunCompleted:
		if len(observations) < 2 {
			return NewError(CodeRunState,
				"conditioning requires at least two observations")
		}
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
