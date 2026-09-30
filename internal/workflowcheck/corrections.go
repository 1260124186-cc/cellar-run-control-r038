package workflowcheck

import (
	"fmt"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// observationCorrections exercises the controlled correction workflow:
// only the tail observation of a run that has not entered final calculation
// can be corrected; original values, reasons, and timestamps are preserved;
// invalid corrections fail atomically; and completion afterwards uses the
// corrected values.
func observationCorrections(api *client) error {
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
	base := time.Now().UTC().Add(10 * time.Second)
	points := []struct {
		gravity     float64
		temperature float64
	}{
		{gravity: 1.050, temperature: 20.0},
		{gravity: 1.028, temperature: 20.5},
		{gravity: 1.014, temperature: 19.8},
	}
	version := started.Run.Version
	for index, point := range points {
		sequence := index + 1
		response, callErr := api.call("POST", "/v1/runs/"+runID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       point.gravity,
				"temperature_c": point.temperature,
				"observed_at":   verifiedAt(base, sequence),
				"note":          fmt.Sprintf("observation %d", sequence),
			}, ptrInt64(version))
		if callErr != nil {
			return callErr
		}
		if err := requireStatus(response, 200, "append observation"); err != nil {
			return err
		}
		var run runView
		if err := response.decode(&run); err != nil {
			return err
		}
		version = run.Version
	}

	// Capture the tail observation's receipt time before any correction.
	beforeResponse, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(beforeResponse, 200, "read run before correction"); err != nil {
		return err
	}
	var before runView
	if err := beforeResponse.decode(&before); err != nil {
		return err
	}
	originalReceivedAt := before.Observations[2].ReceivedAt

	// First controlled correction of the tail observation (sequence 3).
	firstCorrect, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.016,
			"temperature_c": 20.1,
			"observed_at":   base.Add(35 * time.Second),
			"note":          "corrected sample",
			"reason":        "field thermometer was misread",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(firstCorrect, 200, "correct tail observation"); err != nil {
		return err
	}
	var corrected runView
	if err := firstCorrect.decode(&corrected); err != nil {
		return err
	}
	version = corrected.Version
	tail := corrected.Observations[2]
	if tail.Gravity != 1.016 || tail.TemperatureC != 20.1 || tail.Note != "corrected sample" {
		return errInvalidWorkflow("correction did not replace the tail observation values")
	}
	if len(tail.Corrections) != 1 {
		return errInvalidWorkflow("correction history was not recorded")
	}
	history := tail.Corrections[0]
	if history.PreviousGravity != 1.014 ||
		history.PreviousTemperatureC != 19.8 ||
		history.PreviousNote != "observation 3" ||
		history.Reason != "field thermometer was misread" ||
		history.CorrectedAt.IsZero() ||
		!history.PreviousObservedAt.Equal(verifiedAt(base, 3)) {
		return errInvalidWorkflow("correction history did not preserve the original facts")
	}
	if !tail.ReceivedAt.Equal(originalReceivedAt) || tail.ReceivedAt.IsZero() {
		return errInvalidWorkflow("correction must retain the original receipt time")
	}

	// A second correction chains history and keeps the previously corrected value.
	secondCorrect, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.018,
			"temperature_c": 20.2,
			"observed_at":   base.Add(38 * time.Second),
			"note":          "corrected sample",
			"reason":        "lab cross check",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(secondCorrect, 200, "correct tail observation twice"); err != nil {
		return err
	}
	if err := secondCorrect.decode(&corrected); err != nil {
		return err
	}
	version = corrected.Version
	if len(corrected.Observations) != 3 || corrected.Observations[2].Gravity != 1.018 {
		return errInvalidWorkflow("second correction did not apply cleanly")
	}
	if len(corrected.Observations[2].Corrections) != 2 ||
		corrected.Observations[2].Corrections[1].PreviousGravity != 1.016 {
		return errInvalidWorkflow("second correction lost the prior correction history")
	}

	// Failure: only the tail can be corrected.
	nonTail, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      2,
			"gravity":       1.029,
			"temperature_c": 20.0,
			"observed_at":   base.Add(25 * time.Second),
			"note":          "",
			"reason":        "attempt on non-tail",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(nonTail, 409, string(domain.CodeObservationConflict),
		"correct a non-tail observation"); err != nil {
		return err
	}

	// Failure: gravity would rise too far above the preceding observation.
	rising, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.050,
			"temperature_c": 20.0,
			"observed_at":   base.Add(39 * time.Second),
			"note":          "",
			"reason":        "attempt rising gravity",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(rising, 409, string(domain.CodeObservationConflict),
		"correct with excessive gravity rise"); err != nil {
		return err
	}

	// Failure: observed time would break strict ordering with the predecessor.
	outOfOrder, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.017,
			"temperature_c": 20.0,
			"observed_at":   base.Add(5 * time.Second),
			"note":          "",
			"reason":        "attempt out-of-order time",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(outOfOrder, 409, string(domain.CodeObservationConflict),
		"correct with a non-monotonic time"); err != nil {
		return err
	}

	// Failure: out-of-range values are validation errors.
	outOfRange, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.4,
			"temperature_c": 20.0,
			"observed_at":   base.Add(36 * time.Second),
			"note":          "",
			"reason":        "attempt out-of-range gravity",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(outOfRange, 400, string(domain.CodeInvalidInput),
		"correct with an out-of-range gravity"); err != nil {
		return err
	}

	// Failure: a reason is mandatory.
	missingReason, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.017,
			"temperature_c": 20.0,
			"observed_at":   base.Add(36 * time.Second),
			"note":          "",
			"reason":        "   ",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(missingReason, 400, string(domain.CodeInvalidInput),
		"correct without a reason"); err != nil {
		return err
	}

	// Failure: a stale aggregate version is rejected.
	stale, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      3,
			"gravity":       1.017,
			"temperature_c": 20.0,
			"observed_at":   base.Add(36 * time.Second),
			"note":          "",
			"reason":        "attempt with a stale version",
		}, ptrInt64(version-1))
	if err != nil {
		return err
	}
	if err := requireCode(stale, 412, string(domain.CodeRevisionConflict),
		"correct with a stale version"); err != nil {
		return err
	}

	// Every failed correction must be atomic: facts, history, and version stay.
	readResponse, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(readResponse, 200, "read run after failed corrections"); err != nil {
		return err
	}
	var readBack runView
	if err := readResponse.decode(&readBack); err != nil {
		return err
	}
	if readBack.Version != version {
		return errInvalidWorkflow("failed corrections must not advance the run version")
	}
	if len(readBack.Observations) != 3 || readBack.Observations[2].Gravity != 1.018 {
		return errInvalidWorkflow("failed corrections changed the current observation facts")
	}
	if len(readBack.Observations[2].Corrections) != 2 {
		return errInvalidWorkflow("failed corrections altered the correction history")
	}

	// The corrected series still accepts appends and keeps strict time order.
	appendFourth, err := api.call("POST", "/v1/runs/"+runID+"/observations",
		map[string]any{
			"sequence":      4,
			"gravity":       1.012,
			"temperature_c": 19.5,
			"observed_at":   base.Add(45 * time.Second),
			"note":          "observation 4",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(appendFourth, 200, "append after corrections"); err != nil {
		return err
	}
	if err := appendFourth.decode(&corrected); err != nil {
		return err
	}
	version = corrected.Version

	// Conditioning remains reachable: the observation-count gate re-established.
	conditioningResponse, err := api.call("POST", "/v1/runs/"+runID+"/conditioning",
		nil, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioningResponse, 200, "begin conditioning after corrections"); err != nil {
		return err
	}
	if err := conditioningResponse.decode(&corrected); err != nil {
		return err
	}
	if err := requireRunState(corrected, domain.RunConditioning); err != nil {
		return err
	}
	version = corrected.Version

	// A correction is still allowed during conditioning because final metrics
	// have not yet been computed.
	conditioningCorrect, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      4,
			"gravity":       1.013,
			"temperature_c": 19.6,
			"observed_at":   base.Add(48 * time.Second),
			"note":          "observation 4",
			"reason":        "conditioning cross check",
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioningCorrect, 200, "correct tail during conditioning"); err != nil {
		return err
	}
	if err := conditioningCorrect.decode(&corrected); err != nil {
		return err
	}
	if err := requireRunState(corrected, domain.RunConditioning); err != nil {
		return err
	}
	if corrected.Observations[3].Gravity != 1.013 {
		return errInvalidWorkflow("conditioning correction did not apply")
	}
	version = corrected.Version

	// Completion then uses the corrected tail observation for final metrics.
	completeResponse, err := api.call("POST", "/v1/runs/"+runID+"/complete",
		nil, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(completeResponse, 200, "complete corrected run"); err != nil {
		return err
	}
	var completed pairView
	if err := completeResponse.decode(&completed); err != nil {
		return err
	}
	if err := requireRunState(completed.Run, domain.RunCompleted); err != nil {
		return err
	}
	if completed.Run.Metrics == nil ||
		completed.Run.Metrics.ObservationCount != 4 ||
		completed.Run.Metrics.FinalGravity != 1.01 ||
		completed.Run.Metrics.ApparentAttenuation <= 0 ||
		completed.Run.Metrics.EstimatedAlcoholABV <= 0 {
		return errInvalidWorkflow("completion did not use the corrected observation")
	}

	// Run details expose both current facts and the full correction history.
	detailsResponse, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(detailsResponse, 200, "read completed corrected run"); err != nil {
		return err
	}
	var details runView
	if err := detailsResponse.decode(&details); err != nil {
		return err
	}
	if details.Metrics == nil || len(details.Observations) != 4 {
		return errInvalidWorkflow("run details lost current facts")
	}
	if len(details.Observations[2].Corrections) != 2 ||
		len(details.Observations[3].Corrections) != 1 {
		return errInvalidWorkflow("run details did not carry the correction history")
	}

	// Once final calculation ran, corrections are permanently closed.
	afterComplete, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      4,
			"gravity":       1.011,
			"temperature_c": 19.6,
			"observed_at":   base.Add(49 * time.Second),
			"note":          "",
			"reason":        "attempt after completion",
		}, ptrInt64(details.Version))
	if err != nil {
		return err
	}
	if err := requireCode(afterComplete, 409, string(domain.CodeRunState),
		"correct after completion"); err != nil {
		return err
	}

	return assertCorrectionClosedForAbortedRun(api, formula.ID)
}

// assertCorrectionClosedForAbortedRun covers the remaining closed states:
// a run without observations and an aborted run both reject corrections.
func assertCorrectionClosedForAbortedRun(api *client, formulaID string) error {
	abortVessel, err := createCheckVessel(api, "VSL-304", 300)
	if err != nil {
		return err
	}
	started, err := createStartedRun(api, formulaID, abortVessel.ID, "RUN-CORRECTION-ABORT")
	if err != nil {
		return err
	}
	runID := started.Run.ID
	base := time.Now().UTC().Add(10 * time.Second)

	noTarget, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      1,
			"gravity":       1.050,
			"temperature_c": 20.0,
			"observed_at":   verifiedAt(base, 1),
			"note":          "",
			"reason":        "attempt with no observations",
		}, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(noTarget, 409, string(domain.CodeObservationConflict),
		"correct a run without observations"); err != nil {
		return err
	}

	appended, err := api.call("POST", "/v1/runs/"+runID+"/observations",
		map[string]any{
			"sequence":      1,
			"gravity":       1.050,
			"temperature_c": 20.0,
			"observed_at":   verifiedAt(base, 1),
			"note":          "observation 1",
		}, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(appended, 200, "append before abort"); err != nil {
		return err
	}
	var run runView
	if err := appended.decode(&run); err != nil {
		return err
	}

	aborted, err := api.call("POST", "/v1/runs/"+runID+"/abort",
		nil, ptrInt64(run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(aborted, 200, "abort run"); err != nil {
		return err
	}
	var abortedPair pairView
	if err := aborted.decode(&abortedPair); err != nil {
		return err
	}
	run = abortedPair.Run

	afterAbort, err := api.call("POST", "/v1/runs/"+runID+"/observations/correct",
		map[string]any{
			"sequence":      1,
			"gravity":       1.049,
			"temperature_c": 20.0,
			"observed_at":   base.Add(15 * time.Second),
			"note":          "",
			"reason":        "attempt after abort",
		}, ptrInt64(run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(afterAbort, 409, string(domain.CodeRunState),
		"correct after abort"); err != nil {
		return err
	}
	return nil
}
