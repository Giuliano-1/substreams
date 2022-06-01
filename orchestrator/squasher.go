package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/streamingfast/substreams/block"
	"github.com/streamingfast/substreams/state"
)

// Squasher produces _complete_ stores, by merging backing partial stores.
type Squasher struct {
	squashables          map[string]*Squashable
	storeSaveInterval    uint64
	targetExclusiveBlock uint64

	notifier Notifier

	lock sync.Mutex
}

type SquasherOption func(s *Squasher)

func WithNotifier(notifier Notifier) SquasherOption {
	return func(s *Squasher) {
		s.notifier = notifier
	}
}

func NewSquasher(ctx context.Context, storageState *StorageState, stores map[string]*state.Store, storeSaveInterval uint64, targetExclusiveBlock uint64, opts ...SquasherOption) (*Squasher, error) {
	squashables := map[string]*Squashable{}
	for _, store := range stores {
		lastKVSavedBlock := storageState.lastBlocks[store.Name]
		storagePresent := lastKVSavedBlock != 0
		if !storagePresent {
			squashables[store.Name] = NewSquashable(store.CloneStructure(store.ModuleInitialBlock), targetExclusiveBlock, storeSaveInterval, store.ModuleInitialBlock)
		} else {
			r := &block.Range{
				StartBlock:        store.ModuleInitialBlock,
				ExclusiveEndBlock: lastKVSavedBlock, // This ASSUMES we have scheduled jobs that are going to pipe us new results in.
			}
			squish, err := store.LoadFrom(ctx, r)
			if err != nil {
				return nil, fmt.Errorf("loading store %q: range %s: %w", store.Name, r, err)
			}
			squashables[store.Name] = NewSquashable(squish, targetExclusiveBlock, storeSaveInterval, lastKVSavedBlock)
		}
	}

	squasher := &Squasher{
		squashables:          squashables,
		storeSaveInterval:    storeSaveInterval,
		targetExclusiveBlock: targetExclusiveBlock,
	}

	for _, opt := range opts {
		opt(squasher)
	}

	return squasher, nil
}

func (s *Squasher) Squash(ctx context.Context, moduleName string, outgoingReqRange *block.Range) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	squashable, ok := s.squashables[moduleName]
	if !ok {
		return fmt.Errorf("module %q was not found in squashables module registry", moduleName)
	}

	return squashable.squash(ctx, outgoingReqRange, s.notifier)
}

func (s *Squasher) StoresReady() (out map[string]*state.Store, err error) {
	out = map[string]*state.Store{}
	var errs []string
	for _, v := range s.squashables {
		if !v.targetReached {
			errs = append(errs, fmt.Sprintf("module %q target not reached", v.name))
		}
		if !v.IsEmpty() {
			errs = append(errs, fmt.Sprintf("module %q missing ranges %s", v.name, v.ranges))
		}
		out[v.store.Name] = v.store
	}
	if len(errs) != 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

type Squashable struct {
	name                   string
	store                  *state.Store
	ranges                 block.Ranges // Ranges split in `storeSaveInterval` chunks
	storeSaveInterval      uint64
	targetExclusiveBlock   uint64
	nextExpectedStartBlock uint64

	targetReached bool
}

func NewSquashable(initialStore *state.Store, targetExclusiveBlock, storeSaveInterval, nextExpectedStartBlock uint64) *Squashable {
	return &Squashable{
		name:                   initialStore.Name,
		store:                  initialStore,
		storeSaveInterval:      storeSaveInterval,
		targetExclusiveBlock:   targetExclusiveBlock,
		nextExpectedStartBlock: nextExpectedStartBlock,
	}
}

func (s *Squashable) squash(ctx context.Context, blockRange *block.Range, notifier Notifier) error {
	zlog.Info("cumulating squash request range", zap.String("module", s.name), zap.Object("request_range", blockRange))

	if err := s.cumulateRange(ctx, blockRange); err != nil {
		return fmt.Errorf("cumulate range: %w", err)
	}

	if err := s.mergeAvailablePartials(ctx, notifier); err != nil {
		return fmt.Errorf("merging partials: %w", err)
	}

	return nil
}

func (s *Squashable) cumulateRange(ctx context.Context, blockRange *block.Range) error {
	splitBlockRanges := blockRange.Split(s.storeSaveInterval)
	for _, splitBlockRange := range splitBlockRanges {
		if splitBlockRange.StartBlock < s.store.ModuleInitialBlock {
			return fmt.Errorf("module %q: received a squash request for a start block %d prior to the module's initial block %d", s.name, splitBlockRange.StartBlock, s.store.ModuleInitialBlock)
		}
		if blockRange.ExclusiveEndBlock < s.store.StoreInitialBlock {
			// Otherwise, risks stalling the merging (as ranges are
			// sorted, and only the first is checked for contiguousness)
			continue
		}
		zlog.Debug("appending range", zap.Stringer("split_block_range", splitBlockRange))
		s.ranges = append(s.ranges, splitBlockRange)
	}
	sort.Sort(s.ranges)
	return nil
}

func (s *Squashable) mergeAvailablePartials(ctx context.Context, notifier Notifier) error {
	zlog.Info("squashing", zap.String("module_name", s.store.Name))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if len(s.ranges) == 0 {
			break
		}

		squashableRange := s.ranges[0]

		if squashableRange.StartBlock < s.nextExpectedStartBlock {
			return fmt.Errorf("module %q: non contiguous ranges were added to the store squasher, expected %d, got %d, ranges: %s", s.name, s.nextExpectedStartBlock, squashableRange.StartBlock, s.ranges)
		}
		if s.nextExpectedStartBlock != squashableRange.StartBlock {
			break
		}

		zlog.Debug("found range to merge", zap.Stringer("squashable", s), zap.Stringer("squashable_range", squashableRange))

		nextStore, err := s.store.LoadFrom(ctx, squashableRange)
		if err != nil {
			return fmt.Errorf("initializing next partial store %q: %w", s.name, err)
		}

		err = s.store.Merge(nextStore)
		if err != nil {
			return fmt.Errorf("merging: %s", err)
		}

		s.nextExpectedStartBlock = squashableRange.ExclusiveEndBlock

		endsOnBoundary := squashableRange.ExclusiveEndBlock%s.storeSaveInterval == 0
		if endsOnBoundary {
			err = s.store.WriteState(ctx, squashableRange.ExclusiveEndBlock)
			if err != nil {
				return fmt.Errorf("writing state: %w", err)
			}
		} else {
			err = nextStore.DeleteStore(ctx, squashableRange.ExclusiveEndBlock)
			if err != nil {
				zlog.Warn("deleting partial file", zap.Error(err))
			}
		}

		s.ranges = s.ranges[1:]

		if squashableRange.ExclusiveEndBlock == s.targetExclusiveBlock {
			s.targetReached = true
		}

		if notifier != nil {
			notifier.Notify(s.store.Name, squashableRange.ExclusiveEndBlock)
		}
	}

	return nil
}

func (s *Squashable) IsEmpty() bool {
	return len(s.ranges) == 0
}

func (s *Squashable) String() string {
	var add string
	if s.targetReached {
		add = " (target reached)"
	}
	return fmt.Sprintf("%s%s: [%s]", s.name, add, s.ranges)
}

type Squashables []*Squashable

func (s Squashables) String() string {
	var rs []string
	for _, i := range s {
		rs = append(rs, i.String())
	}
	return strings.Join(rs, ", ")
}
