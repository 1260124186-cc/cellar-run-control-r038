package service

import "github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"

func (s *Service) AppendObservation(
	id string,
	input ObservationInput,
) (domain.FermentationRun, error) {
	receivedAt := s.clock.Now()
	var prepared preparedRunCommit
	err := s.store.View(func(view domain.Snapshot) error {
		snapshot := &view
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		observation := domain.GravityObservation{
			RunID:        run.ID,
			Sequence:     input.Sequence,
			Gravity:      input.Gravity,
			TemperatureC: input.TemperatureC,
			ObservedAt:   input.ObservedAt.UTC(),
			Note:         input.Note,
			ReceivedAt:   receivedAt,
		}
		plan, err := run.PrepareObservation(observation)
		if err != nil {
			return err
		}
		prepared = preparedRunCommit{Run: plan}
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	prepared.withCommitTime(s.clock.Now())
	if err := s.store.Execute(prepared.apply); err != nil {
		return domain.FermentationRun{}, err
	}
	return prepared.Run.Run.Clone(), nil
}

// CorrectObservation applies a controlled correction to the tail observation
// of a run that has not entered final calculation. The version gate, the
// domain correction, and persistence all happen in one serialized transaction,
// so a stale revision, a non-tail sequence, or any invalid corrected value
// fails atomically without partial writes.
func (s *Service) CorrectObservation(
	id string,
	input ObservationCorrectionInput,
) (domain.FermentationRun, error) {
	correctedAt := s.clock.Now()
	var updated domain.FermentationRun
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		candidate := domain.GravityObservation{
			Gravity:      input.Gravity,
			TemperatureC: input.TemperatureC,
			ObservedAt:   input.ObservedAt.UTC(),
			Note:         input.Note,
		}
		if err := run.CorrectObservation(input.Sequence, candidate,
			input.Reason, correctedAt); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return updated, nil
}

func (s *Service) BeginConditioning(
	id string,
	input VersionInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		if err := run.BeginConditioning(s.clock.Now()); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return updated, nil
}

func (s *Service) CompleteRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, error) {
	var prepared preparedRunCommit
	err := s.store.View(func(view domain.Snapshot) error {
		snapshot := &view
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		formula, ok := snapshot.Formulas[run.FormulaID]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "attached formula was not found")
		}
		plan, err := run.PrepareCompletion(formula.TargetOriginalGravity)
		if err != nil {
			return err
		}
		if plan.Run.VesselID == nil {
			return domain.NewError(domain.CodeRunState, "run has no vessel")
		}
		vessel, ok := snapshot.Vessels[*plan.Run.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if err := vessel.StartCleaning(plan.Run.ID, s.clock.Now()); err != nil {
			return err
		}
		prepared = preparedRunCommit{Run: plan, Vessel: &vessel}
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	prepared.withCommitTime(s.clock.Now())
	if err := s.store.Execute(prepared.apply); err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return prepared.Run.Run.Clone(), *prepared.Vessel, nil
}
