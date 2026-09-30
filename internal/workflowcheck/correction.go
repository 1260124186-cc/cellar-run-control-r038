package workflowcheck

import (
	"fmt"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// correctionBase sits inside the one-minute future tolerance accepted for
// observation timestamps while remaining safely after any run start.
var correctionBase = time.Now().UTC().Add(10 * time.Second)

// observationCorrection drives the controlled observation-correction workflow
// against the real HTTP handler. It verifies that only the trailing
// observation of a run before final calculation can be corrected, that the
// original value, reason, and correction time are retained, that sequence,
// time order, gravity movement, and state gates are re-established, and that
// stale, non-tail, or destructive corrections fail atomically.
func observationCorrection(api *client) error {
	formula, err := ensureApprovedFormula(api, "Correction Formula")
	if err != nil {
		return err
	}
	vessel, err := createCheckVessel(api, "VSL-303", 300)
	if err != nil {
		return err
	}
	started, err := createStartedRun(api, formula.ID, vessel.ID, "RUN-CORRECTIONS")
	if err != nil {
		return err
	}
	runID := started.Run.ID

	if err := appendCorrectionFixtures(api, runID, started.Run.Version, 1.050, 1.030, 1.012); err != nil {
		return err
	}

	read, err := readCorrectionRun(api, runID)
	if err != nil {
		return err
	}
	version := read.Version // three appends: start v3 -> v6
	if len(read.Observations) != 3 {
		return errInvalidWorkflow("expected three observations before corrections")
	}

	// All of the following must fail atomically and leave the run untouched.
	if err := rejectCorrectionFailures(api, runID, version); err != nil {
		return err
	}

	unchanged, err := readCorrectionRun(api, runID)
	if err != nil {
		return err
	}
	if unchanged.Version != version {
		return errInvalidWorkflow("failed corrections changed the run version")
	}
	if len(unchanged.Corrections) != 0 {
		return errInvalidWorkflow("failed corrections recorded correction history")
	}
	if last := unchanged.Observations[2]; last.Gravity != 1.012 {
		return errInvalidWorkflow("failed corrections modified the current observation")
	}

	// First controlled correction: original value, reason, and time retained.
	first, err := sendCorrection(api, runID, 3, ptrInt64(version), map[string]any{
		"gravity":       1.010,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"note":          "corrected reading",
		"reason":        "hydrometer was misread",
	})
	if err != nil {
		return err
	}
	if err := requireStatus(first, 200, "correct latest observation"); err != nil {
		return err
	}
	var corrected runView
	if err := first.decode(&corrected); err != nil {
		return err
	}
	if err := requireCurrentObservation(corrected, 3, 1.010); err != nil {
		return err
	}
	if corrected.Version != version+1 {
		return errInvalidWorkflow("successful correction did not advance the version")
	}
	if err := requireCorrectionHistory(corrected, 3, 1.012, 1.010, "hydrometer was misread"); err != nil {
		return err
	}

	// A second correction accumulates history and keeps the full chain.
	second, err := sendCorrection(api, runID, 3, ptrInt64(corrected.Version), map[string]any{
		"gravity":       1.009,
		"temperature_c": 20.1,
		"observed_at":   verifiedAt(correctionBase, 3),
		"note":          "corrected reading",
		"reason":        "second scale adjustment",
	})
	if err != nil {
		return err
	}
	if err := requireStatus(second, 200, "correct latest observation twice"); err != nil {
		return err
	}
	if err := second.decode(&corrected); err != nil {
		return err
	}
	if err := requireCurrentObservation(corrected, 3, 1.009); err != nil {
		return err
	}
	if len(corrected.Corrections) != 2 {
		return errInvalidWorkflow("second correction did not retain full history")
	}
	latest := corrected.Corrections[1]
	if latest.Before.Gravity != 1.010 || latest.After.Gravity != 1.009 {
		return errInvalidWorkflow("second correction did not chain from the first")
	}

	// Corrections remain allowed in conditioning and must re-establish the gate.
	conditioning, err := api.call("POST", "/v1/runs/"+runID+"/conditioning",
		nil, ptrInt64(corrected.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioning, 200, "enter conditioning after corrections"); err != nil {
		return err
	}
	if err := conditioning.decode(&corrected); err != nil {
		return err
	}
	if err := requireRunState(corrected, domain.RunConditioning); err != nil {
		return err
	}
	third, err := sendCorrection(api, runID, 3, ptrInt64(corrected.Version), map[string]any{
		"gravity":       1.016,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"note":          "corrected reading",
		"reason":        "conditioning cross-check",
	})
	if err != nil {
		return err
	}
	if err := requireStatus(third, 200, "correct observation during conditioning"); err != nil {
		return err
	}
	if err := third.decode(&corrected); err != nil {
		return err
	}
	if err := requireRunState(corrected, domain.RunConditioning); err != nil {
		return errInvalidWorkflow("correction broke the conditioning state gate")
	}
	if len(corrected.Corrections) != 3 {
		return errInvalidWorkflow("conditioning correction did not preserve history")
	}

	// Completion uses the corrected current fact for final calculation.
	complete, err := api.call("POST", "/v1/runs/"+runID+"/complete",
		nil, ptrInt64(corrected.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(complete, 200, "complete corrected run"); err != nil {
		return err
	}
	var completed pairView
	if err := complete.decode(&completed); err != nil {
		return err
	}
	if completed.Run.Metrics == nil ||
		completed.Run.Metrics.ObservationCount != 3 ||
		completed.Run.Metrics.FinalGravity != 1.02 ||
		completed.Run.Metrics.ApparentAttenuation <= 0 ||
		completed.Run.Metrics.EstimatedAlcoholABV <= 0 {
		return errInvalidWorkflow("completion did not use the corrected final gravity")
	}

	// After final calculation corrections must fail atomically.
	locked, err := sendCorrection(api, runID, 3, ptrInt64(completed.Run.Version), map[string]any{
		"gravity":       1.010,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"note":          "too late",
		"reason":        "post-completion attempt",
	})
	if err != nil {
		return err
	}
	if err := requireCode(locked, 409, string(domain.CodeRunState),
		"reject correction after completion"); err != nil {
		return err
	}
	finalRun, err := readCorrectionRun(api, runID)
	if err != nil {
		return err
	}
	if err := requireRunState(finalRun, domain.RunCompleted); err != nil {
		return err
	}
	if err := requireCurrentObservation(finalRun, 3, 1.016); err != nil {
		return err
	}
	if len(finalRun.Corrections) != 3 || finalRun.Metrics == nil {
		return errInvalidWorkflow("rejected post-completion correction mutated history")
	}

	return verifyAbortedRunIsLocked(api, formula.ID)
}

// rejectCorrectionFailures exercises every required atomic failure mode while
// the run is fermenting and carries three observations.
func rejectCorrectionFailures(api *client, runID string, version int64) error {
	nonTail, err := sendCorrection(api, runID, 2, ptrInt64(version), map[string]any{
		"gravity":       1.029,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 2),
		"reason":        "cannot touch an interior observation",
	})
	if err != nil {
		return err
	}
	if err := requireCode(nonTail, 409, string(domain.CodeObservationConflict),
		"reject non-tail observation correction"); err != nil {
		return err
	}

	stale, err := sendCorrection(api, runID, 3, ptrInt64(version-1), map[string]any{
		"gravity":       1.010,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"reason":        "stale revision",
	})
	if err != nil {
		return err
	}
	if err := requireCode(stale, 412, string(domain.CodeRevisionConflict),
		"reject stale correction version"); err != nil {
		return err
	}

	missingReason, err := sendCorrection(api, runID, 3, ptrInt64(version), map[string]any{
		"gravity":       1.010,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"reason":        "   ",
	})
	if err != nil {
		return err
	}
	if err := requireCode(missingReason, 400, string(domain.CodeInvalidInput),
		"reject correction without a reason"); err != nil {
		return err
	}

	risingGravity, err := sendCorrection(api, runID, 3, ptrInt64(version), map[string]any{
		"gravity":       1.050,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"reason":        "gravity would jump above the limit",
	})
	if err != nil {
		return err
	}
	if err := requireCode(risingGravity, 409, string(domain.CodeObservationConflict),
		"reject gravity jump above the previous value"); err != nil {
		return err
	}

	nonMonotonic, err := sendCorrection(api, runID, 3, ptrInt64(version), map[string]any{
		"gravity":       1.010,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 2),
		"reason":        "time would not stay ordered",
	})
	if err != nil {
		return err
	}
	if err := requireCode(nonMonotonic, 409, string(domain.CodeObservationConflict),
		"reject correction breaking time order"); err != nil {
		return err
	}

	outOfRange, err := sendCorrection(api, runID, 3, ptrInt64(version), map[string]any{
		"gravity":       1.4,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 3),
		"reason":        "gravity outside the allowed range",
	})
	if err != nil {
		return err
	}
	if err := requireCode(outOfRange, 400, string(domain.CodeInvalidInput),
		"reject correction with out-of-range gravity"); err != nil {
		return err
	}
	return nil
}

// verifyAbortedRunIsLocked corrects a single observation (no predecessor),
// then confirms an aborted run is immutable even though it never completed.
func verifyAbortedRunIsLocked(api *client, formulaID string) error {
	vessel, err := createCheckVessel(api, "VSL-304", 250)
	if err != nil {
		return err
	}
	started, err := createStartedRun(api, formulaID, vessel.ID, "RUN-CORRECTION-ABORT")
	if err != nil {
		return err
	}
	runID := started.Run.ID

	appendResponse, err := api.call("POST", "/v1/runs/"+runID+"/observations",
		map[string]any{
			"sequence":      1,
			"gravity":       1.050,
			"temperature_c": 20.0,
			"observed_at":   verifiedAt(correctionBase, 1),
			"note":          "only point",
		}, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(appendResponse, 200, "append sole observation"); err != nil {
		return err
	}
	var single runView
	if err := appendResponse.decode(&single); err != nil {
		return err
	}

	only, err := sendCorrection(api, runID, 1, ptrInt64(single.Version), map[string]any{
		"gravity":       1.048,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 1),
		"note":          "only point corrected",
		"reason":        "single-point recalibration",
	})
	if err != nil {
		return err
	}
	if err := requireStatus(only, 200, "correct the sole observation"); err != nil {
		return err
	}
	if err := only.decode(&single); err != nil {
		return err
	}
	if err := requireCurrentObservation(single, 1, 1.048); err != nil {
		return err
	}
	if err := requireCorrectionHistory(single, 1, 1.050, 1.048, "single-point recalibration"); err != nil {
		return err
	}

	abort, err := api.call("POST", "/v1/runs/"+runID+"/abort",
		nil, ptrInt64(single.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(abort, 200, "abort corrected run"); err != nil {
		return err
	}
	var aborted pairView
	if err := abort.decode(&aborted); err != nil {
		return err
	}

	locked, err := sendCorrection(api, runID, 1, ptrInt64(aborted.Run.Version), map[string]any{
		"gravity":       1.047,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(correctionBase, 1),
		"reason":        "post-abort attempt",
	})
	if err != nil {
		return err
	}
	if err := requireCode(locked, 409, string(domain.CodeRunState),
		"reject correction after abort"); err != nil {
		return err
	}
	readBack, err := readCorrectionRun(api, runID)
	if err != nil {
		return err
	}
	if err := requireRunState(readBack, domain.RunAborted); err != nil {
		return err
	}
	if err := requireCurrentObservation(readBack, 1, 1.048); err != nil {
		return err
	}
	if len(readBack.Corrections) != 1 {
		return errInvalidWorkflow("rejected post-abort correction mutated history")
	}
	return nil
}

func appendCorrectionFixtures(api *client, runID string, startVersion int64, gravities ...float64) error {
	version := startVersion
	for index, gravity := range gravities {
		sequence := index + 1
		response, err := api.call("POST", "/v1/runs/"+runID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       gravity,
				"temperature_c": 20.0,
				"observed_at":   verifiedAt(correctionBase, sequence),
				"note":          fmt.Sprintf("observation %d", sequence),
			}, ptrInt64(version))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 200, "append correction fixture"); err != nil {
			return err
		}
		var run runView
		if err := response.decode(&run); err != nil {
			return err
		}
		version = run.Version
	}
	return nil
}

func sendCorrection(
	api *client,
	runID string,
	sequence int,
	expectedVersion *int64,
	payload map[string]any,
) (response, error) {
	return api.call("POST",
		fmt.Sprintf("/v1/runs/%s/observations/%d/correction", runID, sequence),
		payload, expectedVersion)
}

func readCorrectionRun(api *client, runID string) (runView, error) {
	response, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 200, "read correction run"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}

func requireCurrentObservation(run runView, sequence int, gravity float64) error {
	if len(run.Observations) != sequence {
		return fmt.Errorf("expected %d current observations, got %d",
			sequence, len(run.Observations))
	}
	current := run.Observations[sequence-1]
	if current.Sequence != sequence || current.Gravity != gravity {
		return fmt.Errorf("current observation %d = %v, want gravity %v",
			sequence, current.Gravity, gravity)
	}
	return nil
}

func requireCorrectionHistory(
	run runView,
	sequence int,
	beforeGravity float64,
	afterGravity float64,
	reason string,
) error {
	if len(run.Corrections) == 0 {
		return errInvalidWorkflow("run details did not explain correction history")
	}
	record := run.Corrections[len(run.Corrections)-1]
	if record.Sequence != sequence ||
		record.Before.Gravity != beforeGravity ||
		record.After.Gravity != afterGravity ||
		record.Reason != reason {
		return fmt.Errorf("correction history mismatch: seq=%d before=%v after=%v reason=%q",
			record.Sequence, record.Before.Gravity, record.After.Gravity, record.Reason)
	}
	if record.Before.ObservedAt.IsZero() || record.After.ObservedAt.IsZero() {
		return errInvalidWorkflow("correction history lost observation times")
	}
	if record.CorrectedAt.IsZero() {
		return errInvalidWorkflow("correction history did not record when it happened")
	}
	if record.Before.ReceivedAt.Equal(record.CorrectedAt) {
		return errInvalidWorkflow("original receipt time was not retained separately")
	}
	return nil
}
