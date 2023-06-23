package stage

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/streamingfast/substreams/block"
	"github.com/streamingfast/substreams/orchestrator/loop"
	"github.com/streamingfast/substreams/pipeline/outputmodules"
	"github.com/streamingfast/substreams/reqctx"
	"github.com/streamingfast/substreams/storage/store"
	"github.com/streamingfast/substreams/utils"
)

// NOTE:
// Would we have an internal StoreMap here where there's an
// store.FullKV _and_ a State, so this thing would be top-level
// here in the `Stages`, it would keep track of what's happening with
// its internal `store.FullKV`, and the merging state.
// The `ModuleState` would be merely pointing to that Map,
// or a Map of "MergeableStore" ? with a completedSegments, etc, etc..
// most of the things in the `modstate` ?
// and they'd live here: Stages::storeMap: map[storeName]*MergeableStore
// any incoming message that a merged store finished, would funnel
// to its Stage, and would keep track of all those MergeableStore, see if
// all of its modules are completed for that stage, and then send the signal
// that the Stage is completed, kicking off the next layer of jobs.

type Stages struct {
	ctx     context.Context
	logger  *zap.Logger
	traceID string

	segmenter *block.Segmenter

	stages []*Stage

	// segmentStates is a matrix of segment and stages
	segmentStates []stageStates // segmentStates[offsetSegment][StageIndex]

	// If you're processing at 12M blocks, offset 12,000 segments so you don't need to allocate 12k empty elements.
	// Any previous segment is assumed to have completed successfully, and any stores that we sync'd prior to this offset
	// are assumed to have been either fully loaded, or merged up until this offset.
	segmentOffset int
}
type stageStates []UnitState

func NewStages(
	ctx context.Context,
	outputGraph *outputmodules.Graph,
	segmenter *block.Segmenter,
	storeConfigs store.ConfigMap,
	traceID string,
) (out *Stages) {
	logger := reqctx.Logger(ctx)

	stagedModules := outputGraph.StagedUsedModules()
	lastIndex := len(stagedModules) - 1
	out = &Stages{
		ctx:           ctx,
		traceID:       traceID,
		segmenter:     segmenter,
		segmentOffset: segmenter.IndexForStartBlock(outputGraph.LowestInitBlock()),
	}
	for idx, mods := range stagedModules {
		isLastStage := idx == lastIndex
		kind := stageKind(mods)
		if kind == KindMap && !isLastStage {
			continue
		}

		var moduleStates []*ModuleState
		lowestStageInitBlock := mods[0].InitialBlock
		for _, mod := range mods {
			modSegmenter := segmenter.WithInitialBlock(mod.InitialBlock)
			modState := NewModuleState(logger, mod.Name, modSegmenter, storeConfigs[mod.Name])
			moduleStates = append(moduleStates, modState)

			lowestStageInitBlock = utils.MinOf(lowestStageInitBlock, mod.InitialBlock)
		}

		stageSegmenter := segmenter.WithInitialBlock(lowestStageInitBlock)
		stage := NewStage(idx, kind, stageSegmenter, moduleStates)
		out.stages = append(out.stages, stage)
	}
	return out
}

func (s *Stages) AllStagesFinished() bool {
	// AllStagesFinished should rather look to the
	// Stages and confirm that all the Stores in the `moduleState`
	// have been properly merged and that we're ready to continue
	// to the Linear stage.
	//
	// FIXME: The simple fact that the unit is marked Completed
	// will not work when we move it to FullKV exists.
	// It's not the same thing

	lastSegment := s.segmenter.LastIndex()
	lastSegmentIndex := lastSegment - s.segmentOffset
	if len(s.segmentStates) < lastSegmentIndex {
		return false
	}

	for idx, stage := range s.stages {
		if stage.kind == KindMap {
			continue
		}
		if s.getState(Unit{Segment: lastSegment, Stage: idx}) != UnitCompleted {
			return false
		}
	}
	return true
}

func (s *Stages) InitialProgressMessages() map[string]block.Ranges {
	out := make(map[string]block.Ranges)
	for _, stage := range s.stages {
		// TODO: pluck initial messages from segmentCompleted
		// and all of the PartialPresent or Complete stages, if that's
		// possible
		for _, mod := range stage.moduleStates {
			out[mod.name] = append(out[mod.name], mod.InitialRange())
		}
	}
	return out
}

func (s *Stages) CmdStartMerge() loop.Cmd {
	var cmds []loop.Cmd
	for idx := range s.stages {
		cmds = append(cmds, s.CmdMerge(idx))
	}
	return loop.Batch(cmds...)
}

func (s *Stages) CmdMerge(stageIdx int) loop.Cmd {
	if s.AllStagesFinished() {
		// FIXME: if we have multiple stages that signal
		// StoresCompleted because let's say they were all initialized
		// and were ready from the get go, would produce multiple
		// such messages. But the only downside would be that we schedule
		// multiple Quit messages.. but then the further ones are ignored.
		return func() loop.Msg {
			return MsgMergeStoresCompleted{}
		}
	}
	// FIXME: bound checks are necessary here, or in the caller
	// to make sure the previous segment is completed (see MarkSegmentMerging's
	// internal check).
	stage := s.stages[stageIdx]
	mergeUnit := stage.nextUnit()

	if mergeUnit.Segment > s.segmenter.LastIndex() {
		// TODO: we could affect a state for the whole Stage here
		// and the AllStagesFinished() could be a quick
		// check of that state instead of plucking through the
		// Unit matrix.
		return nil // We're done here.
	}

	if !s.previousUnitComplete(mergeUnit) {
		return nil
	}

	s.MarkSegmentMerging(mergeUnit)

	return func() loop.Msg {
		if err := s.multiSquash(stage, mergeUnit); err != nil {
			return MsgMergeFailed{Unit: mergeUnit, Error: err}
		}
		return MsgMergeFinished{Unit: mergeUnit}
	}
}

func (s *Stages) MergeCompleted(mergeUnit Unit) {
	s.stages[mergeUnit.Stage].markSegmentCompleted(mergeUnit.Segment)
	s.markSegmentCompleted(mergeUnit)
}

func (s *Stages) getState(u Unit) UnitState {
	return s.segmentStates[u.Segment-s.segmentOffset][u.Stage]
}

func (s *Stages) setState(u Unit, state UnitState) {
	s.segmentStates[u.Segment-s.segmentOffset][u.Stage] = state
}

func (s *Stages) WaitAsyncWork() error {
	for _, stage := range s.stages {
		if err := stage.asyncWork.Wait(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stages) NextJob() (Unit, *block.Range) {
	// TODO: before calling NextJob, keep a small reserve (10% ?) of workers
	//  so that when a job finishes, it can start immediately a potentially
	//  higher priority one (we'll go do all those first-level jobs
	//  but we want to keep the diagonal balanced).
	// TODO: Another option is to have an algorithm that doesn't return a job
	//  right away when there are too much jobs scheduled before others
	//  in a given stage.

	// FIXME: eventually, we can start from s.segmentsOffset, and push `segmentsOffset`
	//  each time contiguous segments are completed for all stages.
	segmentIdx := s.segmenter.FirstIndex()
	for {
		if len(s.segmentStates) <= segmentIdx-s.segmentOffset {
			s.growSegments()
		}
		if segmentIdx > s.segmenter.LastIndex() {
			break
		}
		for stageIdx := len(s.stages) - 1; stageIdx >= 0; stageIdx-- {
			unit := Unit{Segment: segmentIdx, Stage: stageIdx}
			segmentState := s.getState(unit)
			if segmentState != UnitPending {
				continue
			}
			if segmentIdx < s.stages[stageIdx].segmenter.FirstIndex() {
				// Don't process stages where all modules's initial blocks are only later
				continue
			}
			if !s.dependenciesCompleted(unit) {
				continue
			}

			s.markSegmentScheduled(unit)
			return unit, s.segmenter.Range(unit.Segment)
		}
		segmentIdx++
	}
	return Unit{}, nil
}

func (s *Stages) growSegments() {
	by := len(s.segmentStates)
	if by == 0 {
		by = 2
	}
	for i := 0; i < by; i++ {
		s.segmentStates = append(s.segmentStates, make([]UnitState, len(s.stages)))
	}
}

func (s *Stages) dependenciesCompleted(u Unit) bool {
	if u.Segment <= s.stages[u.Stage].segmenter.FirstIndex() {
		return true
	}
	if u.Stage == 0 {
		return true
	}
	for i := u.Stage - 1; i >= 0; i-- {
		if s.getState(Unit{Segment: u.Segment - 1, Stage: i}) != UnitCompleted {
			return false
		}
	}
	return true
}

func (s *Stages) previousUnitComplete(u Unit) bool {
	if u.Segment-s.segmentOffset <= 0 {
		return true
	}
	return s.getState(Unit{Segment: u.Segment - 1, Stage: u.Stage}) == UnitCompleted
}

func (s *Stages) FinalStoreMap(atBlock uint64) store.Map {
	out := store.NewMap()
	for _, stage := range s.stages {
		for _, modState := range stage.moduleStates {
			fullKV, err := modState.getStore(s.ctx, atBlock)
			if err != nil {
				// TODO: do proper error propagation
				panic(fmt.Errorf("stores didn't sync up properly, expected store %q to be at block %d but was at %d: %w", modState.name, atBlock, modState.lastBlockInStore, err))
			}
			out[modState.name] = fullKV
		}
	}
	return out
}
