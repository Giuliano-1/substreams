package store

import (
	"context"
	"fmt"
	"io"

	"github.com/streamingfast/derr"
	"github.com/streamingfast/dstore"
	"go.uber.org/zap"

	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/streamingfast/substreams/storage/store/marshaller"
)

type Config struct {
	name       string
	moduleHash string
	objStore   dstore.Store

	moduleInitialBlock uint64
	updatePolicy       pbsubstreams.Module_KindStore_UpdatePolicy
	valueType          string

	appendLimit    uint64
	totalSizeLimit uint64
	itemSizeLimit  uint64
}

func NewConfig(name string, moduleInitialBlock uint64, moduleHash string, updatePolicy pbsubstreams.Module_KindStore_UpdatePolicy, valueType string, store dstore.Store) (*Config, error) {
	subStore, err := store.SubStore(fmt.Sprintf("%s/states", moduleHash))
	if err != nil {
		return nil, fmt.Errorf("creating sub store: %w", err)
	}

	return &Config{
		name:               name,
		updatePolicy:       updatePolicy,
		valueType:          valueType,
		objStore:           subStore,
		moduleInitialBlock: moduleInitialBlock,
		moduleHash:         moduleHash,
		appendLimit:        8_388_608,     // 8MiB = 8 * 1024 * 1024,
		totalSizeLimit:     1_073_741_824, // 1GiB
		itemSizeLimit:      10_485_760,    // 10MiB
	}, nil
}

func (c *Config) newBaseStore(logger *zap.Logger) *baseStore {
	return &baseStore{
		Config:     c,
		kv:         make(map[string][]byte),
		logger:     logger.Named("store").With(zap.String("store_name", c.name), zap.String("module_hash", c.moduleHash)),
		marshaller: marshaller.Default(),
	}
}

func (c *Config) Name() string {
	return c.name
}

func (c *Config) ModuleHash() string {
	return c.moduleHash
}

func (c *Config) ValueType() string {
	return c.valueType
}

func (c *Config) UpdatePolicy() pbsubstreams.Module_KindStore_UpdatePolicy {
	return c.updatePolicy
}

func (c *Config) ModuleInitialBlock() uint64 {
	return c.moduleInitialBlock
}

func (c *Config) NewFullKV(logger *zap.Logger) *FullKV {
	return &FullKV{c.newBaseStore(logger), "N/A"}
}

func (c *Config) NewPartialKV(initialBlock uint64, logger *zap.Logger) *PartialKV {
	return &PartialKV{
		baseStore:    c.newBaseStore(logger),
		initialBlock: initialBlock,
		seen:         make(map[string]bool),
	}
}

func (c *Config) FileSize(ctx context.Context, fileInfo *FileInfo) (int64, error) {
	var size int64
	err := derr.RetryContext(ctx, 3, func(ctx context.Context) error {
		attr, err := c.objStore.ObjectAttributes(ctx, fileInfo.Filename)
		if err != nil {
			return fmt.Errorf("getting object attributes: %w", err)
		}

		size = attr.Size
		return nil
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (c *Config) ListSnapshotFiles(ctx context.Context, below uint64) (files []*FileInfo, err error) {
	if below == 0 {
		return nil, nil
	}
	err = derr.RetryContext(ctx, 3, func(ctx context.Context) error {
		files = nil
		if err := c.objStore.Walk(ctx, "", func(filename string) (err error) {
			fileInfo, ok := parseFileName(filename)
			if !ok {
				return nil
			}
			if fileInfo.StartBlock >= below {
				return io.EOF
			}
			files = append(files, fileInfo)
			return nil
		}); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("walking snapshots: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
