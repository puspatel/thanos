// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package compact

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/golang/groupcache/singleflight"
	"github.com/oklog/ulid/v2"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/thanos-io/objstore"
	"golang.org/x/sync/errgroup"

	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/compact/downsample"
	"github.com/thanos-io/thanos/pkg/errutil"
	"github.com/thanos-io/thanos/pkg/runutil"
	"github.com/thanos-io/thanos/pkg/tracing"
)

type ResolutionLevel int64

const (
	ResolutionLevelRaw = ResolutionLevel(downsample.ResLevel0)
	ResolutionLevel5m  = ResolutionLevel(downsample.ResLevel1)
	ResolutionLevel1h  = ResolutionLevel(downsample.ResLevel2)
)

const (
	// DedupAlgorithmPenalty is the penalty based compactor series merge algorithm.
	// This is the same as the online deduplication of querier except counter reset handling.
	DedupAlgorithmPenalty = "penalty"
)

// Syncer synchronizes block metas from a bucket into a local directory.
// It sorts them into compaction groups based on equal label sets.
type Syncer struct {
	logger                   log.Logger
	bkt                      objstore.Bucket
	fetcher                  block.MetadataFetcher
	mtx                      sync.Mutex
	blocks                   map[ulid.ULID]*metadata.Meta
	partial                  map[ulid.ULID]error
	metrics                  *SyncerMetrics
	duplicateBlocksFilter    block.DeduplicateFilter
	ignoreDeletionMarkFilter *block.IgnoreDeletionMarkFilter
	syncMetasTimeout         time.Duration

	g singleflight.Group
}

// SyncerMetrics holds metrics tracked by the syncer. This struct and its fields are exported
// to allow depending projects (eg. Cortex) to implement their own custom syncer while tracking
// compatible metrics.
type SyncerMetrics struct {
	GarbageCollectedBlocks    prometheus.Counter
	GarbageCollections        prometheus.Counter
	GarbageCollectionFailures prometheus.Counter
	GarbageCollectionDuration prometheus.Observer
	BlocksMarkedForDeletion   prometheus.Counter
}

func NewSyncerMetrics(reg prometheus.Registerer, blocksMarkedForDeletion, garbageCollectedBlocks prometheus.Counter) *SyncerMetrics {
	var m SyncerMetrics

	m.GarbageCollectedBlocks = garbageCollectedBlocks
	m.GarbageCollections = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compact_garbage_collection_total",
		Help: "Total number of garbage collection operations.",
	})
	m.GarbageCollectionFailures = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compact_garbage_collection_failures_total",
		Help: "Total number of failed garbage collection operations.",
	})
	m.GarbageCollectionDuration = promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "thanos_compact_garbage_collection_duration_seconds",
		Help:    "Time it took to perform garbage collection iteration.",
		Buckets: []float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9, 20, 30, 60, 90, 120, 240, 360, 720},
	})

	m.BlocksMarkedForDeletion = blocksMarkedForDeletion

	return &m
}

// NewMetaSyncer returns a new Syncer for the given Bucket and directory.
// Blocks must be at least as old as the sync delay for being considered.
func NewMetaSyncer(logger log.Logger, reg prometheus.Registerer, bkt objstore.Bucket, fetcher block.MetadataFetcher, duplicateBlocksFilter block.DeduplicateFilter, ignoreDeletionMarkFilter *block.IgnoreDeletionMarkFilter, blocksMarkedForDeletion, garbageCollectedBlocks prometheus.Counter, syncMetasTimeout time.Duration) (*Syncer, error) {
	return NewMetaSyncerWithMetrics(logger,
		NewSyncerMetrics(reg, blocksMarkedForDeletion, garbageCollectedBlocks),
		bkt,
		fetcher,
		duplicateBlocksFilter,
		ignoreDeletionMarkFilter,
		syncMetasTimeout,
	)
}

func NewMetaSyncerWithMetrics(logger log.Logger, metrics *SyncerMetrics, bkt objstore.Bucket, fetcher block.MetadataFetcher, duplicateBlocksFilter block.DeduplicateFilter, ignoreDeletionMarkFilter *block.IgnoreDeletionMarkFilter, syncMetasTimeout time.Duration) (*Syncer, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &Syncer{
		syncMetasTimeout:         syncMetasTimeout,
		logger:                   logger,
		bkt:                      bkt,
		fetcher:                  fetcher,
		blocks:                   map[ulid.ULID]*metadata.Meta{},
		metrics:                  metrics,
		duplicateBlocksFilter:    duplicateBlocksFilter,
		ignoreDeletionMarkFilter: ignoreDeletionMarkFilter,
	}, nil
}

// UntilNextDownsampling calculates how long it will take until the next downsampling operation.
// Returns an error if there will be no downsampling.
func UntilNextDownsampling(m *metadata.Meta) (time.Duration, error) {
	timeRange := time.Duration((m.MaxTime - m.MinTime) * int64(time.Millisecond))
	switch m.Thanos.Downsample.Resolution {
	case downsample.ResLevel2:
		return time.Duration(0), errors.New("no downsampling")
	case downsample.ResLevel1:
		return time.Duration(downsample.ResLevel2DownsampleRange*time.Millisecond) - timeRange, nil
	case downsample.ResLevel0:
		return time.Duration(downsample.ResLevel1DownsampleRange*time.Millisecond) - timeRange, nil
	default:
		panic(errors.Errorf("invalid resolution %v", m.Thanos.Downsample.Resolution))
	}
}

// SyncMetas synchronizes local state of block metas with what we have in the bucket.
func (s *Syncer) SyncMetas(ctx context.Context) error {
	var cancel func() = func() {}
	if s.syncMetasTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.syncMetasTimeout)
	}
	defer cancel()

	type metasContainer struct {
		metas   map[ulid.ULID]*metadata.Meta
		partial map[ulid.ULID]error
	}

	container, err := s.g.Do("", func() (interface{}, error) {
		metas, partial, err := s.fetcher.Fetch(ctx)
		return metasContainer{metas, partial}, err
	})
	if err != nil {
		return retry(err)
	}
	s.mtx.Lock()
	s.blocks = container.(metasContainer).metas
	s.partial = container.(metasContainer).partial
	s.mtx.Unlock()
	return nil
}

// Partial returns partial blocks since last sync.
func (s *Syncer) Partial() map[ulid.ULID]error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return s.partial
}

// Metas returns loaded metadata blocks since last sync.
func (s *Syncer) Metas() map[ulid.ULID]*metadata.Meta {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	metas := make(map[ulid.ULID]*metadata.Meta, len(s.blocks))
	for k, v := range s.blocks {
		metas[k] = v
	}

	return metas
}

// GarbageCollect marks blocks for deletion from bucket if their data is available as part of a
// block with a higher compaction level.
// Call to SyncMetas function is required to populate duplicateIDs in duplicateBlocksFilter.
func (s *Syncer) GarbageCollect(ctx context.Context) error {
	begin := time.Now()

	// Ignore filter exists before deduplicate filter.
	deletionMarkMap := s.ignoreDeletionMarkFilter.DeletionMarkBlocks()
	duplicateIDs := s.duplicateBlocksFilter.DuplicateIDs()

	// GarbageIDs contains the duplicateIDs, since these blocks can be replaced with other blocks.
	// We also remove ids present in deletionMarkMap since these blocks are already marked for deletion.
	garbageIDs := []ulid.ULID{}
	for _, id := range duplicateIDs {
		if _, exists := deletionMarkMap[id]; exists {
			continue
		}
		garbageIDs = append(garbageIDs, id)
	}

	for _, id := range garbageIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Spawn a new context so we always mark a block for deletion in full on shutdown.
		delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

		level.Info(s.logger).Log("msg", "marking outdated block for deletion", "block", id)
		err := block.MarkForDeletion(delCtx, s.logger, s.bkt, id, "outdated block", s.metrics.BlocksMarkedForDeletion)
		cancel()
		if err != nil {
			s.metrics.GarbageCollectionFailures.Inc()
			return retry(errors.Wrapf(err, "mark block %s for deletion", id))
		}

		// Immediately update our in-memory state so no further call to SyncMetas is needed
		// after running garbage collection.
		s.mtx.Lock()
		delete(s.blocks, id)
		s.mtx.Unlock()
		s.metrics.GarbageCollectedBlocks.Inc()
	}
	s.metrics.GarbageCollections.Inc()
	s.metrics.GarbageCollectionDuration.Observe(time.Since(begin).Seconds())
	return nil
}

// Grouper is responsible to group all known blocks into sub groups which are safe to be
// compacted concurrently.
type Grouper interface {
	// Groups returns the compaction groups for all blocks currently known to the syncer.
	// It creates all groups from the scratch on every call.
	Groups(blocks map[ulid.ULID]*metadata.Meta) (res []*Group, err error)
}

// DefaultGrouper is the Thanos built-in grouper. It groups blocks based on downsample
// resolution and block's labels.
type DefaultGrouper struct {
	bkt                           objstore.Bucket
	logger                        log.Logger
	acceptMalformedIndex          bool
	enableVerticalCompaction      bool
	compactions                   *prometheus.CounterVec
	compactionRunsStarted         *prometheus.CounterVec
	compactionRunsCompleted       *prometheus.CounterVec
	compactionFailures            *prometheus.CounterVec
	verticalCompactions           *prometheus.CounterVec
	garbageCollectedBlocks        prometheus.Counter
	blocksMarkedForDeletion       prometheus.Counter
	blocksMarkedForNoCompact      prometheus.Counter
	hashFunc                      metadata.HashFunc
	blockFilesConcurrency         int
	compactBlocksFetchConcurrency int
}

// NewDefaultGrouper makes a new DefaultGrouper.
func NewDefaultGrouper(
	logger log.Logger,
	bkt objstore.Bucket,
	acceptMalformedIndex bool,
	enableVerticalCompaction bool,
	reg prometheus.Registerer,
	blocksMarkedForDeletion prometheus.Counter,
	garbageCollectedBlocks prometheus.Counter,
	blocksMarkedForNoCompact prometheus.Counter,
	hashFunc metadata.HashFunc,
	blockFilesConcurrency int,
	compactBlocksFetchConcurrency int,
) *DefaultGrouper {
	return &DefaultGrouper{
		bkt:                      bkt,
		logger:                   logger,
		acceptMalformedIndex:     acceptMalformedIndex,
		enableVerticalCompaction: enableVerticalCompaction,
		compactions: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_compact_group_compactions_total",
			Help: "Total number of group compaction attempts that resulted in a new block.",
		}, []string{"resolution"}),
		compactionRunsStarted: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_compact_group_compaction_runs_started_total",
			Help: "Total number of group compaction attempts.",
		}, []string{"resolution"}),
		compactionRunsCompleted: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_compact_group_compaction_runs_completed_total",
			Help: "Total number of group completed compaction runs. This also includes compactor group runs that resulted with no compaction.",
		}, []string{"resolution"}),
		compactionFailures: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_compact_group_compactions_failures_total",
			Help: "Total number of failed group compactions.",
		}, []string{"resolution"}),
		verticalCompactions: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_compact_group_vertical_compactions_total",
			Help: "Total number of group compaction attempts that resulted in a new block based on overlapping blocks.",
		}, []string{"resolution"}),
		blocksMarkedForNoCompact:      blocksMarkedForNoCompact,
		garbageCollectedBlocks:        garbageCollectedBlocks,
		blocksMarkedForDeletion:       blocksMarkedForDeletion,
		hashFunc:                      hashFunc,
		blockFilesConcurrency:         blockFilesConcurrency,
		compactBlocksFetchConcurrency: compactBlocksFetchConcurrency,
	}
}

// NewDefaultGrouperWithMetrics makes a new DefaultGrouper.
func NewDefaultGrouperWithMetrics(
	logger log.Logger,
	bkt objstore.Bucket,
	acceptMalformedIndex bool,
	enableVerticalCompaction bool,
	compactions *prometheus.CounterVec,
	compactionRunsStarted *prometheus.CounterVec,
	compactionRunsCompleted *prometheus.CounterVec,
	compactionFailures *prometheus.CounterVec,
	verticalCompactions *prometheus.CounterVec,
	blocksMarkedForDeletion prometheus.Counter,
	garbageCollectedBlocks prometheus.Counter,
	blocksMarkedForNoCompact prometheus.Counter,
	hashFunc metadata.HashFunc,
	blockFilesConcurrency int,
	compactBlocksFetchConcurrency int,
) *DefaultGrouper {
	return &DefaultGrouper{
		bkt:                           bkt,
		logger:                        logger,
		acceptMalformedIndex:          acceptMalformedIndex,
		enableVerticalCompaction:      enableVerticalCompaction,
		compactions:                   compactions,
		compactionRunsStarted:         compactionRunsStarted,
		compactionRunsCompleted:       compactionRunsCompleted,
		compactionFailures:            compactionFailures,
		verticalCompactions:           verticalCompactions,
		blocksMarkedForNoCompact:      blocksMarkedForNoCompact,
		garbageCollectedBlocks:        garbageCollectedBlocks,
		blocksMarkedForDeletion:       blocksMarkedForDeletion,
		hashFunc:                      hashFunc,
		blockFilesConcurrency:         blockFilesConcurrency,
		compactBlocksFetchConcurrency: compactBlocksFetchConcurrency,
	}
}

// Groups returns the compaction groups for all blocks currently known to the syncer.
// It creates all groups from the scratch on every call.
func (g *DefaultGrouper) Groups(blocks map[ulid.ULID]*metadata.Meta) (res []*Group, err error) {
	groups := map[string]*Group{}
	for _, m := range blocks {
		groupKey := m.Thanos.GroupKey()
		group, ok := groups[groupKey]
		if !ok {
			lbls := labels.FromMap(m.Thanos.Labels)
			resolutionLabel := m.Thanos.ResolutionString()
			group, err = NewGroup(
				log.With(g.logger, "group", fmt.Sprintf("%s@%v", resolutionLabel, lbls.String()), "groupKey", groupKey),
				g.bkt,
				groupKey,
				lbls,
				m.Thanos.Downsample.Resolution,
				g.acceptMalformedIndex,
				g.enableVerticalCompaction,
				g.compactions.WithLabelValues(resolutionLabel),
				g.compactionRunsStarted.WithLabelValues(resolutionLabel),
				g.compactionRunsCompleted.WithLabelValues(resolutionLabel),
				g.compactionFailures.WithLabelValues(resolutionLabel),
				g.verticalCompactions.WithLabelValues(resolutionLabel),
				g.garbageCollectedBlocks,
				g.blocksMarkedForDeletion,
				g.blocksMarkedForNoCompact,
				g.hashFunc,
				g.blockFilesConcurrency,
				g.compactBlocksFetchConcurrency,
			)
			if err != nil {
				return nil, errors.Wrap(err, "create compaction group")
			}
			groups[groupKey] = group
			res = append(res, group)
		}
		if err := group.AppendMeta(m); err != nil {
			return nil, errors.Wrap(err, "add compaction group")
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Key() < res[j].Key()
	})
	return res, nil
}

// Group captures a set of blocks that have the same origin labels and downsampling resolution.
// Those blocks generally contain the same series and can thus efficiently be compacted.
type Group struct {
	logger                        log.Logger
	bkt                           objstore.Bucket
	key                           string
	labels                        labels.Labels
	resolution                    int64
	mtx                           sync.Mutex
	metasByMinTime                []*metadata.Meta
	acceptMalformedIndex          bool
	enableVerticalCompaction      bool
	compactions                   prometheus.Counter
	compactionRunsStarted         prometheus.Counter
	compactionRunsCompleted       prometheus.Counter
	compactionFailures            prometheus.Counter
	verticalCompactions           prometheus.Counter
	groupGarbageCollectedBlocks   prometheus.Counter
	blocksMarkedForDeletion       prometheus.Counter
	blocksMarkedForNoCompact      prometheus.Counter
	hashFunc                      metadata.HashFunc
	blockFilesConcurrency         int
	compactBlocksFetchConcurrency int
	extensions                    any
}

// NewGroup returns a new compaction group.
func NewGroup(
	logger log.Logger,
	bkt objstore.Bucket,
	key string,
	lset labels.Labels,
	resolution int64,
	acceptMalformedIndex bool,
	enableVerticalCompaction bool,
	compactions prometheus.Counter,
	compactionRunsStarted prometheus.Counter,
	compactionRunsCompleted prometheus.Counter,
	compactionFailures prometheus.Counter,
	verticalCompactions prometheus.Counter,
	groupGarbageCollectedBlocks prometheus.Counter,
	blocksMarkedForDeletion prometheus.Counter,
	blocksMarkedForNoCompact prometheus.Counter,
	hashFunc metadata.HashFunc,
	blockFilesConcurrency int,
	compactBlocksFetchConcurrency int,
) (*Group, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	if blockFilesConcurrency <= 0 {
		return nil, errors.Errorf("invalid concurrency level (%d), blockFilesConcurrency level must be > 0", blockFilesConcurrency)
	}

	g := &Group{
		logger:                        logger,
		bkt:                           bkt,
		key:                           key,
		labels:                        lset,
		resolution:                    resolution,
		acceptMalformedIndex:          acceptMalformedIndex,
		enableVerticalCompaction:      enableVerticalCompaction,
		compactions:                   compactions,
		compactionRunsStarted:         compactionRunsStarted,
		compactionRunsCompleted:       compactionRunsCompleted,
		compactionFailures:            compactionFailures,
		verticalCompactions:           verticalCompactions,
		groupGarbageCollectedBlocks:   groupGarbageCollectedBlocks,
		blocksMarkedForDeletion:       blocksMarkedForDeletion,
		blocksMarkedForNoCompact:      blocksMarkedForNoCompact,
		hashFunc:                      hashFunc,
		blockFilesConcurrency:         blockFilesConcurrency,
		compactBlocksFetchConcurrency: compactBlocksFetchConcurrency,
	}
	return g, nil
}

// Key returns an identifier for the group.
func (cg *Group) Key() string {
	return cg.key
}

func (cg *Group) deleteFromGroup(target map[ulid.ULID]struct{}) {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()
	var newGroupMeta []*metadata.Meta
	for _, meta := range cg.metasByMinTime {
		if _, found := target[meta.BlockMeta.ULID]; !found {
			newGroupMeta = append(newGroupMeta, meta)
		}
	}

	cg.metasByMinTime = newGroupMeta
}

// AppendMeta the block with the given meta to the group.
func (cg *Group) AppendMeta(meta *metadata.Meta) error {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()

	if !labels.Equal(cg.labels, labels.FromMap(meta.Thanos.Labels)) {
		return errors.New("block and group labels do not match")
	}
	if cg.resolution != meta.Thanos.Downsample.Resolution {
		return errors.New("block and group resolution do not match")
	}

	cg.metasByMinTime = append(cg.metasByMinTime, meta)
	sort.Slice(cg.metasByMinTime, func(i, j int) bool {
		return cg.metasByMinTime[i].MinTime < cg.metasByMinTime[j].MinTime
	})
	return nil
}

// IDs returns all sorted IDs of blocks in the group.
func (cg *Group) IDs() (ids []ulid.ULID) {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()

	for _, m := range cg.metasByMinTime {
		ids = append(ids, m.ULID)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].Compare(ids[j]) < 0
	})
	return ids
}

// MinTime returns the min time across all group's blocks.
func (cg *Group) MinTime() int64 {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()

	if len(cg.metasByMinTime) > 0 {
		return cg.metasByMinTime[0].MinTime
	}
	return math.MaxInt64
}

// MaxTime returns the max time across all group's blocks.
func (cg *Group) MaxTime() int64 {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()

	max := int64(math.MinInt64)
	for _, m := range cg.metasByMinTime {
		if m.MaxTime > max {
			max = m.MaxTime
		}
	}
	return max
}

// Labels returns the labels that all blocks in the group share.
func (cg *Group) Labels() labels.Labels {
	return cg.labels
}

// Resolution returns the common downsampling resolution of blocks in the group.
func (cg *Group) Resolution() int64 {
	return cg.resolution
}

func (cg *Group) Extensions() any {
	return cg.extensions
}

func (cg *Group) SetExtensions(extensions any) {
	cg.extensions = extensions
}

// CompactProgressMetrics contains Prometheus metrics related to compaction progress.
type CompactProgressMetrics struct {
	NumberOfCompactionRuns   prometheus.Gauge
	NumberOfCompactionBlocks prometheus.Gauge
}

// ProgressCalculator calculates the progress of the compaction process for a given slice of Groups.
type ProgressCalculator interface {
	ProgressCalculate(ctx context.Context, groups []*Group) error
}

// CompactionProgressCalculator contains a planner and ProgressMetrics, which are updated during the compaction simulation process.
type CompactionProgressCalculator struct {
	planner Planner
	*CompactProgressMetrics
}

// NewCompactProgressCalculator creates a new CompactionProgressCalculator.
func NewCompactionProgressCalculator(reg prometheus.Registerer, planner *tsdbBasedPlanner) *CompactionProgressCalculator {
	return &CompactionProgressCalculator{
		planner: planner,
		CompactProgressMetrics: &CompactProgressMetrics{
			NumberOfCompactionRuns: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "thanos_compact_todo_compactions",
				Help: "number of compactions to be done",
			}),
			NumberOfCompactionBlocks: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "thanos_compact_todo_compaction_blocks",
				Help: "number of blocks planned to be compacted",
			}),
		},
	}
}

// ProgressCalculate calculates the number of blocks and compaction runs in the planning process of the given groups.
func (ps *CompactionProgressCalculator) ProgressCalculate(ctx context.Context, groups []*Group) error {
	groupCompactions := make(map[string]int, len(groups))
	groupBlocks := make(map[string]int, len(groups))

	for len(groups) > 0 {
		tmpGroups := make([]*Group, 0, len(groups))
		for _, g := range groups {
			if len(g.IDs()) == 1 {
				continue
			}
			plan, err := ps.planner.Plan(ctx, g.metasByMinTime, nil, g.extensions)
			if err != nil {
				return errors.Wrapf(err, "could not plan")
			}
			if len(plan) == 0 {
				continue
			}
			groupCompactions[g.key]++

			toRemove := make(map[ulid.ULID]struct{}, len(plan))
			metas := make([]*tsdb.BlockMeta, 0, len(plan))
			for _, p := range plan {
				metas = append(metas, &p.BlockMeta)
				toRemove[p.BlockMeta.ULID] = struct{}{}
			}
			g.deleteFromGroup(toRemove)

			groupBlocks[g.key] += len(plan)

			if len(g.metasByMinTime) == 0 {
				continue
			}

			newMeta := tsdb.CompactBlockMetas(ulid.MustNew(uint64(time.Now().Unix()), nil), metas...)
			if err := g.AppendMeta(&metadata.Meta{BlockMeta: *newMeta, Thanos: metadata.Thanos{Downsample: metadata.ThanosDownsample{Resolution: g.Resolution()}, Labels: g.Labels().Map()}}); err != nil {
				return errors.Wrapf(err, "append meta")
			}
			tmpGroups = append(tmpGroups, g)
		}

		groups = tmpGroups
	}

	ps.CompactProgressMetrics.NumberOfCompactionRuns.Set(0)
	ps.CompactProgressMetrics.NumberOfCompactionBlocks.Set(0)

	for key, iters := range groupCompactions {
		ps.CompactProgressMetrics.NumberOfCompactionRuns.Add(float64(iters))
		ps.CompactProgressMetrics.NumberOfCompactionBlocks.Add(float64(groupBlocks[key]))
	}

	return nil
}

// DownsampleProgressMetrics contains Prometheus metrics related to downsampling progress.
type DownsampleProgressMetrics struct {
	NumberOfBlocksDownsampled prometheus.Gauge
}

// DownsampleProgressCalculator contains DownsampleMetrics, which are updated during the downsampling simulation process.
type DownsampleProgressCalculator struct {
	*DownsampleProgressMetrics
}

// NewDownsampleProgressCalculator creates a new DownsampleProgressCalculator.
func NewDownsampleProgressCalculator(reg prometheus.Registerer) *DownsampleProgressCalculator {
	return &DownsampleProgressCalculator{
		DownsampleProgressMetrics: &DownsampleProgressMetrics{
			NumberOfBlocksDownsampled: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "thanos_compact_todo_downsample_blocks",
				Help: "number of blocks to be downsampled",
			}),
		},
	}
}

// ProgressCalculate calculates the number of blocks to be downsampled for the given groups.
func (ds *DownsampleProgressCalculator) ProgressCalculate(ctx context.Context, groups []*Group) error {
	sources5m := map[ulid.ULID]struct{}{}
	sources1h := map[ulid.ULID]struct{}{}
	groupBlocks := make(map[string]int, len(groups))

	for _, group := range groups {
		for _, m := range group.metasByMinTime {
			switch m.Thanos.Downsample.Resolution {
			case downsample.ResLevel0:
				continue
			case downsample.ResLevel1:
				for _, id := range m.Compaction.Sources {
					sources5m[id] = struct{}{}
				}
			case downsample.ResLevel2:
				for _, id := range m.Compaction.Sources {
					sources1h[id] = struct{}{}
				}
			default:
				return errors.Errorf("unexpected downsampling resolution %d", m.Thanos.Downsample.Resolution)
			}

		}
	}

	for _, group := range groups {
		for _, m := range group.metasByMinTime {
			switch m.Thanos.Downsample.Resolution {
			case downsample.ResLevel0:
				missing := false
				for _, id := range m.Compaction.Sources {
					if _, ok := sources5m[id]; !ok {
						missing = true
						break
					}
				}
				if !missing {
					continue
				}

				if m.MaxTime-m.MinTime < downsample.ResLevel1DownsampleRange {
					continue
				}
				groupBlocks[group.key]++
			case downsample.ResLevel1:
				missing := false
				for _, id := range m.Compaction.Sources {
					if _, ok := sources1h[id]; !ok {
						missing = true
						break
					}
				}
				if !missing {
					continue
				}

				if m.MaxTime-m.MinTime < downsample.ResLevel2DownsampleRange {
					continue
				}
				groupBlocks[group.key]++
			}
		}
	}

	ds.DownsampleProgressMetrics.NumberOfBlocksDownsampled.Set(0)
	for _, blocks := range groupBlocks {
		ds.DownsampleProgressMetrics.NumberOfBlocksDownsampled.Add(float64(blocks))
	}

	return nil
}

// RetentionProgressMetrics contains Prometheus metrics related to retention progress.
type RetentionProgressMetrics struct {
	NumberOfBlocksToDelete prometheus.Gauge
}

// RetentionProgressCalculator contains RetentionProgressMetrics, which are updated during the retention simulation process.
type RetentionProgressCalculator struct {
	*RetentionProgressMetrics
	retentionByResolution map[ResolutionLevel]time.Duration
}

// NewRetentionProgressCalculator creates a new RetentionProgressCalculator.
func NewRetentionProgressCalculator(reg prometheus.Registerer, retentionByResolution map[ResolutionLevel]time.Duration) *RetentionProgressCalculator {
	return &RetentionProgressCalculator{
		retentionByResolution: retentionByResolution,
		RetentionProgressMetrics: &RetentionProgressMetrics{
			NumberOfBlocksToDelete: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "thanos_compact_todo_deletion_blocks",
				Help: "number of blocks that have crossed their retention period",
			}),
		},
	}
}

// ProgressCalculate calculates the number of blocks to be retained for the given groups.
func (rs *RetentionProgressCalculator) ProgressCalculate(ctx context.Context, groups []*Group) error {
	groupBlocks := make(map[string]int, len(groups))

	for _, group := range groups {
		for _, m := range group.metasByMinTime {
			retentionDuration := rs.retentionByResolution[ResolutionLevel(m.Thanos.Downsample.Resolution)]
			if retentionDuration.Seconds() == 0 {
				continue
			}
			maxTime := time.Unix(m.MaxTime/1000, 0)
			if time.Now().After(maxTime.Add(retentionDuration)) {
				groupBlocks[group.key]++
			}
		}
	}

	rs.RetentionProgressMetrics.NumberOfBlocksToDelete.Set(0)
	for _, blocks := range groupBlocks {
		rs.RetentionProgressMetrics.NumberOfBlocksToDelete.Add(float64(blocks))
	}

	return nil
}

// Planner returns blocks to compact.
type Planner interface {
	// Plan returns a list of blocks that should be compacted into single one.
	// The blocks can be overlapping. The provided metadata has to be ordered by minTime.
	Plan(ctx context.Context, metasByMinTime []*metadata.Meta, errChan chan error, extensions any) ([]*metadata.Meta, error)
}

type BlockDeletableChecker interface {
	CanDelete(group *Group, blockID ulid.ULID) bool
}

type DefaultBlockDeletableChecker struct {
}

func (c DefaultBlockDeletableChecker) CanDelete(_ *Group, _ ulid.ULID) bool {
	return true
}

type CompactionLifecycleCallback interface {
	PreCompactionCallback(ctx context.Context, logger log.Logger, group *Group, toCompactBlocks []*metadata.Meta) error
	PostCompactionCallback(ctx context.Context, logger log.Logger, group *Group, blockID ulid.ULID) error
	GetBlockPopulator(ctx context.Context, logger log.Logger, group *Group) (tsdb.BlockPopulator, error)
}

type DefaultCompactionLifecycleCallback struct {
}

func (c DefaultCompactionLifecycleCallback) PreCompactionCallback(_ context.Context, logger log.Logger, cg *Group, toCompactBlocks []*metadata.Meta) error {
	// Due to #183 we verify that none of the blocks in the plan have overlapping sources.
	// This is one potential source of how we could end up with duplicated chunks.
	uniqueSources := map[ulid.ULID]struct{}{}
	for _, m := range toCompactBlocks {
		for _, s := range m.Compaction.Sources {
			if _, ok := uniqueSources[s]; ok {
				if !cg.enableVerticalCompaction {
					return halt(errors.Errorf("overlapping sources detected for plan %v", toCompactBlocks))
				}
				level.Warn(logger).Log("msg", "overlapping sources detected for plan", "duplicated_block", s, "to_compact_blocks", fmt.Sprintf("%v", toCompactBlocks))
			}
			uniqueSources[s] = struct{}{}
		}
	}
	return nil
}

func (c DefaultCompactionLifecycleCallback) PostCompactionCallback(_ context.Context, _ log.Logger, _ *Group, _ ulid.ULID) error {
	return nil
}

func (c DefaultCompactionLifecycleCallback) GetBlockPopulator(_ context.Context, _ log.Logger, _ *Group) (tsdb.BlockPopulator, error) {
	return tsdb.DefaultBlockPopulator{}, nil
}

// Compactor provides compaction against an underlying storage of time series data.
// It is similar to tsdb.Compactor but only relevant methods are kept. Plan and Write are removed.
// TODO(bwplotka): Split the Planner from Compactor on upstream as well, so we can import it.
type Compactor interface {
	// Compact runs compaction against the provided directories. Must
	// only be called concurrently with results of Plan().
	// Can optionally pass a list of already open blocks,
	// to avoid having to reopen them.
	// Prometheus always return one or no block. The interface allows returning more than one
	// block for downstream users to experiment with compactor.
	// When one resulting Block has 0 samples
	//  * No block is written.
	//  * The source dirs are marked Deletable.
	//  * Block is not included in the result.
	Compact(dest string, dirs []string, open []*tsdb.Block) ([]ulid.ULID, error)
	CompactWithBlockPopulator(dest string, dirs []string, open []*tsdb.Block, blockPopulator tsdb.BlockPopulator) ([]ulid.ULID, error)
}

// Compact plans and runs a single compaction against the group. The compacted result
// is uploaded into the bucket the blocks were retrieved from.
func (cg *Group) Compact(ctx context.Context, dir string, planner Planner, comp Compactor, blockDeletableChecker BlockDeletableChecker, compactionLifecycleCallback CompactionLifecycleCallback) (shouldRerun bool, compIDs []ulid.ULID, rerr error) {
	cg.compactionRunsStarted.Inc()

	subDir := filepath.Join(dir, cg.Key())

	defer func() {
		// Leave the compact directory for inspection if it is a halt error
		// or if it is not then so that possibly we would not have to download everything again.
		if rerr != nil {
			return
		}
		if err := os.RemoveAll(subDir); err != nil {
			level.Error(cg.logger).Log("msg", "failed to remove compaction group work directory", "path", subDir, "err", err)
		}
	}()

	if err := os.MkdirAll(subDir, 0750); err != nil {
		return false, nil, errors.Wrap(err, "create compaction group dir")
	}

	defer func() {
		if p := recover(); p != nil {
			var sb strings.Builder

			cgIDs := cg.IDs()
			for i, blid := range cgIDs {
				_, _ = sb.WriteString(blid.String())
				if i < len(cgIDs)-1 {
					_, _ = sb.WriteString(",")
				}
			}
			rerr = fmt.Errorf("panicked while compacting %s: %v", sb.String(), p)
		}
	}()

	errChan := make(chan error, 1)
	err := tracing.DoInSpanWithErr(ctx, "compaction_group", func(ctx context.Context) (err error) {
		shouldRerun, compIDs, err = cg.compact(ctx, subDir, planner, comp, blockDeletableChecker, compactionLifecycleCallback, errChan)
		return err
	}, opentracing.Tags{"group.key": cg.Key()})
	errChan <- err
	close(errChan)
	if err != nil {
		cg.compactionFailures.Inc()
		return false, nil, err
	}
	cg.compactionRunsCompleted.Inc()
	return shouldRerun, compIDs, nil
}

// Issue347Error is a type wrapper for errors that should invoke repair process for broken block.
type Issue347Error struct {
	err error

	id ulid.ULID
}

func issue347Error(err error, brokenBlock ulid.ULID) Issue347Error {
	return Issue347Error{err: err, id: brokenBlock}
}

func (e Issue347Error) Error() string {
	return e.err.Error()
}

// IsIssue347Error returns true if the base error is a Issue347Error.
func IsIssue347Error(err error) bool {
	_, ok := errors.Cause(err).(Issue347Error)
	return ok
}

// OutOfOrderChunkError is a type wrapper for OOO chunk error from validating block index.
type OutOfOrderChunksError struct {
	err error
	id  ulid.ULID
}

func (e OutOfOrderChunksError) Error() string {
	return e.err.Error()
}

func outOfOrderChunkError(err error, brokenBlock ulid.ULID) OutOfOrderChunksError {
	return OutOfOrderChunksError{err: err, id: brokenBlock}
}

// IsOutOfOrderChunkError returns true if the base error is a OutOfOrderChunkError.
func IsOutOfOrderChunkError(err error) bool {
	_, ok := errors.Cause(err).(OutOfOrderChunksError)
	return ok
}

// HaltError is a type wrapper for errors that should halt any further progress on compactions.
type HaltError struct {
	err error
}

func halt(err error) HaltError {
	return HaltError{err: err}
}

func (e HaltError) Error() string {
	return e.err.Error()
}

func (e HaltError) Unwrap() error {
	return errors.Cause(e.err)
}

// IsHaltError returns true if the base error is a HaltError.
// If a multierror is passed, any halt error will return true.
func IsHaltError(err error) bool {
	if multiErr, ok := errors.Cause(err).(errutil.NonNilMultiRootError); ok {
		for _, err := range multiErr {
			if _, ok := errors.Cause(err).(HaltError); ok {
				return true
			}
		}
		return false
	}

	_, ok := errors.Cause(err).(HaltError)
	return ok
}

// RetryError is a type wrapper for errors that should trigger warning log and retry whole compaction loop, but aborting
// current compaction further progress.
type RetryError struct {
	err error
}

func NewRetryError(err error) error {
	return retry(err)
}

func retry(err error) error {
	if IsHaltError(err) {
		return err
	}
	return RetryError{err: err}
}

func (e RetryError) Error() string {
	return e.err.Error()
}

func (e RetryError) Unwrap() error {
	return errors.Cause(e.err)
}

// IsRetryError returns true if the base error is a RetryError.
// If a multierror is passed, all errors must be retriable.
func IsRetryError(err error) bool {
	if multiErr, ok := errors.Cause(err).(errutil.NonNilMultiRootError); ok {
		for _, err := range multiErr {
			if _, ok := errors.Cause(err).(RetryError); !ok {
				return false
			}
		}
		return true
	}

	_, ok := errors.Cause(err).(RetryError)
	return ok
}

func (cg *Group) areBlocksOverlapping(include *metadata.Meta, exclude ...*metadata.Meta) error {
	var (
		metas      []tsdb.BlockMeta
		excludeMap = map[ulid.ULID]struct{}{}
	)

	for _, meta := range exclude {
		excludeMap[meta.ULID] = struct{}{}
	}

	for _, m := range cg.metasByMinTime {
		if _, ok := excludeMap[m.ULID]; ok {
			continue
		}
		metas = append(metas, m.BlockMeta)
	}

	if include != nil {
		metas = append(metas, include.BlockMeta)
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].MinTime < metas[j].MinTime
	})
	if overlaps := tsdb.OverlappingBlocks(metas); len(overlaps) > 0 {
		return errors.Errorf("overlaps found while gathering blocks. %s", overlaps)
	}
	return nil
}

// RepairIssue347 repairs the https://github.com/prometheus/tsdb/issues/347 issue when having issue347Error.
func RepairIssue347(ctx context.Context, logger log.Logger, bkt objstore.Bucket, blocksMarkedForDeletion prometheus.Counter, issue347Err error) error {
	ie, ok := errors.Cause(issue347Err).(Issue347Error)
	if !ok {
		return errors.Errorf("Given error is not an issue347 error: %v", issue347Err)
	}

	level.Info(logger).Log("msg", "Repairing block broken by https://github.com/prometheus/tsdb/issues/347", "id", ie.id, "err", issue347Err)

	tmpdir, err := os.MkdirTemp("", fmt.Sprintf("repair-issue-347-id-%s-", ie.id))
	if err != nil {
		return err
	}

	defer func() {
		if err := os.RemoveAll(tmpdir); err != nil {
			level.Warn(logger).Log("msg", "failed to remote tmpdir", "err", err, "tmpdir", tmpdir)
		}
	}()

	bdir := filepath.Join(tmpdir, ie.id.String())
	if err := block.Download(ctx, logger, bkt, ie.id, bdir); err != nil {
		return retry(errors.Wrapf(err, "download block %s", ie.id))
	}

	meta, err := metadata.ReadFromDir(bdir)
	if err != nil {
		return errors.Wrapf(err, "read meta from %s", bdir)
	}

	resid, err := block.Repair(ctx, logger, tmpdir, ie.id, metadata.CompactorRepairSource, block.IgnoreIssue347OutsideChunk)
	if err != nil {
		return errors.Wrapf(err, "repair failed for block %s", ie.id)
	}

	// Verify repaired id before uploading it.
	if err := block.VerifyIndex(ctx, logger, filepath.Join(tmpdir, resid.String(), block.IndexFilename), meta.MinTime, meta.MaxTime); err != nil {
		return errors.Wrapf(err, "repaired block is invalid %s", resid)
	}

	level.Info(logger).Log("msg", "uploading repaired block", "newID", resid)
	if err = block.Upload(ctx, logger, bkt, filepath.Join(tmpdir, resid.String()), metadata.NoneFunc); err != nil {
		return retry(errors.Wrapf(err, "upload of %s failed", resid))
	}

	level.Info(logger).Log("msg", "deleting broken block", "id", ie.id)

	// Spawn a new context so we always mark a block for deletion in full on shutdown.
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// TODO(bplotka): Issue with this will introduce overlap that will halt compactor. Automate that (fix duplicate overlaps caused by this).
	if err := block.MarkForDeletion(delCtx, logger, bkt, ie.id, "source of repaired block", blocksMarkedForDeletion); err != nil {
		return errors.Wrapf(err, "marking old block %s for deletion has failed", ie.id)
	}
	return nil
}

func (cg *Group) compact(ctx context.Context, dir string, planner Planner, comp Compactor, blockDeletableChecker BlockDeletableChecker, compactionLifecycleCallback CompactionLifecycleCallback, errChan chan error) (bool, []ulid.ULID, error) {
	cg.mtx.Lock()
	defer cg.mtx.Unlock()

	// Check for overlapped blocks.
	overlappingBlocks := false
	if err := cg.areBlocksOverlapping(nil); err != nil {
		// TODO(bwplotka): It would really nice if we could still check for other overlaps than replica. In fact this should be checked
		// in syncer itself. Otherwise with vertical compaction enabled we will sacrifice this important check.
		if !cg.enableVerticalCompaction {
			return false, nil, halt(errors.Wrap(err, "pre compaction overlap check"))
		}

		overlappingBlocks = true
	}

	var toCompact []*metadata.Meta
	if err := tracing.DoInSpanWithErr(ctx, "compaction_planning", func(ctx context.Context) (e error) {
		toCompact, e = planner.Plan(ctx, cg.metasByMinTime, errChan, cg.extensions)
		return e
	}); err != nil {
		return false, nil, errors.Wrap(err, "plan compaction")
	}
	if len(toCompact) == 0 {
		// Nothing to do.
		return false, nil, nil
	}

	level.Info(cg.logger).Log("msg", "compaction available and planned", "plan", fmt.Sprintf("%v", toCompact))

	// Once we have a plan we need to download the actual data.
	groupCompactionBegin := time.Now()
	begin := groupCompactionBegin

	if err := compactionLifecycleCallback.PreCompactionCallback(ctx, cg.logger, cg, toCompact); err != nil {
		return false, nil, errors.Wrapf(err, "failed to run pre compaction callback for plan: %s", fmt.Sprintf("%v", toCompact))
	}
	level.Info(cg.logger).Log("msg", "finished running pre compaction callback; downloading blocks", "duration", time.Since(begin), "duration_ms", time.Since(begin).Milliseconds(), "plan", fmt.Sprintf("%v", toCompact))

	begin = time.Now()
	g, errCtx := errgroup.WithContext(ctx)
	g.SetLimit(cg.compactBlocksFetchConcurrency)

	toCompactDirs := make([]string, 0, len(toCompact))
	for _, m := range toCompact {
		bdir := filepath.Join(dir, m.ULID.String())
		func(ctx context.Context, meta *metadata.Meta) {
			g.Go(func() error {
				start := time.Now()
				if err := tracing.DoInSpanWithErr(ctx, "compaction_block_download", func(ctx context.Context) error {
					return block.Download(ctx, cg.logger, cg.bkt, meta.ULID, bdir, objstore.WithFetchConcurrency(cg.blockFilesConcurrency))
				}, opentracing.Tags{"block.id": meta.ULID}); err != nil {
					return retry(errors.Wrapf(err, "download block %s", meta.ULID))
				}
				level.Debug(cg.logger).Log("msg", "downloaded block", "block", meta.ULID.String(), "duration", time.Since(start), "duration_ms", time.Since(start).Milliseconds())

				start = time.Now()
				// Ensure all input blocks are valid.
				var stats block.HealthStats
				if err := tracing.DoInSpanWithErr(ctx, "compaction_block_health_stats", func(ctx context.Context) (e error) {
					stats, e = block.GatherIndexHealthStats(ctx, cg.logger, filepath.Join(bdir, block.IndexFilename), meta.MinTime, meta.MaxTime)
					return e
				}, opentracing.Tags{"block.id": meta.ULID}); err != nil {
					return errors.Wrapf(err, "gather index issues for block %s", bdir)
				}

				if err := stats.CriticalErr(); err != nil {
					return halt(errors.Wrapf(err, "block with not healthy index found %s; Compaction level %v; Labels: %v", bdir, meta.Compaction.Level, meta.Thanos.Labels))
				}

				if err := stats.OutOfOrderChunksErr(); err != nil {
					return outOfOrderChunkError(errors.Wrapf(err, "blocks with out-of-order chunks are dropped from compaction:  %s", bdir), meta.ULID)
				}

				if err := stats.Issue347OutsideChunksErr(); err != nil {
					return issue347Error(errors.Wrapf(err, "invalid, but reparable block %s", bdir), meta.ULID)
				}

				if err := stats.OutOfOrderLabelsErr(); !cg.acceptMalformedIndex && err != nil {
					return errors.Wrapf(err,
						"block id %s, try running with --debug.accept-malformed-index", meta.ULID)
				}
				level.Debug(cg.logger).Log("msg", "verified block", "block", meta.ULID.String(), "duration", time.Since(start), "duration_ms", time.Since(start).Milliseconds())
				return nil
			})
		}(errCtx, m)

		toCompactDirs = append(toCompactDirs, bdir)
	}
	sourceBlockStr := fmt.Sprintf("%v", toCompactDirs)

	if err := g.Wait(); err != nil {
		return false, nil, err
	}

	level.Info(cg.logger).Log("msg", "downloaded and verified blocks; compacting blocks", "duration", time.Since(begin), "duration_ms", time.Since(begin).Milliseconds(), "plan", sourceBlockStr)

	begin = time.Now()
	var compIDs []ulid.ULID
	if err := tracing.DoInSpanWithErr(ctx, "compaction", func(ctx context.Context) (e error) {
		populateBlockFunc, e := compactionLifecycleCallback.GetBlockPopulator(ctx, cg.logger, cg)
		if e != nil {
			return e
		}
		compIDs, e = comp.CompactWithBlockPopulator(dir, toCompactDirs, nil, populateBlockFunc)
		return e
	}); err != nil {
		return false, nil, halt(errors.Wrapf(err, "compact blocks %v", toCompactDirs))
	}
	if len(compIDs) == 0 {
		// No compacted blocks means all compacted blocks are of no sample.
		level.Info(cg.logger).Log("msg", "no compacted blocks, deleting source blocks", "blocks", sourceBlockStr)
		for _, meta := range toCompact {
			if meta.Stats.NumSamples == 0 {
				if err := cg.deleteBlock(meta.ULID, filepath.Join(dir, meta.ULID.String()), blockDeletableChecker); err != nil {
					level.Warn(cg.logger).Log("msg", "failed to mark for deletion an empty block found during compaction", "block", meta.ULID)
				}
			}
		}
		// Even though no compacted blocks, there may be more work to do.
		return true, nil, nil
	}
	cg.compactions.Inc()
	if overlappingBlocks {
		cg.verticalCompactions.Inc()
	}
	compIDStrings := make([]string, 0, len(compIDs))
	for _, compID := range compIDs {
		compIDStrings = append(compIDStrings, compID.String())
	}
	compIDStrs := fmt.Sprintf("%v", compIDStrings)
	level.Info(cg.logger).Log("msg", "compacted blocks", "new", compIDStrs,
		"duration", time.Since(begin), "duration_ms", time.Since(begin).Milliseconds(), "overlapping_blocks", overlappingBlocks, "blocks", sourceBlockStr)

	for _, compID := range compIDs {
		bdir := filepath.Join(dir, compID.String())
		index := filepath.Join(bdir, block.IndexFilename)

		if err := os.Remove(filepath.Join(bdir, "tombstones")); err != nil {
			return false, nil, errors.Wrap(err, "remove tombstones")
		}

		newMeta, err := metadata.ReadFromDir(bdir)
		if err != nil {
			return false, nil, errors.Wrap(err, "read new meta")
		}

		var stats block.HealthStats
		// Ensure the output block is valid.
		err = tracing.DoInSpanWithErr(ctx, "compaction_verify_index", func(ctx context.Context) error {
			stats, err = block.GatherIndexHealthStats(ctx, cg.logger, index, newMeta.MinTime, newMeta.MaxTime)
			if err != nil {
				return err
			}
			return stats.AnyErr()
		})
		if !cg.acceptMalformedIndex && err != nil {
			return false, nil, halt(errors.Wrapf(err, "invalid result block %s", bdir))
		}

		thanosMeta := metadata.Thanos{
			Labels:       cg.labels.Map(),
			Downsample:   metadata.ThanosDownsample{Resolution: cg.resolution},
			Source:       metadata.CompactorSource,
			SegmentFiles: block.GetSegmentFiles(bdir),
			Extensions:   cg.extensions,
		}
		if stats.ChunkMaxSize > 0 {
			thanosMeta.IndexStats.ChunkMaxSize = stats.ChunkMaxSize
		}
		if stats.SeriesMaxSize > 0 {
			thanosMeta.IndexStats.SeriesMaxSize = stats.SeriesMaxSize
		}
		newMeta, err = metadata.InjectThanos(cg.logger, bdir, thanosMeta, nil)
		if err != nil {
			return false, nil, errors.Wrapf(err, "failed to finalize the block %s", bdir)
		}
		// Ensure the output block is not overlapping with anything else,
		// unless vertical compaction is enabled.
		if !cg.enableVerticalCompaction {
			if err := cg.areBlocksOverlapping(newMeta, toCompact...); err != nil {
				return false, nil, halt(errors.Wrapf(err, "resulted compacted block %s overlaps with something", bdir))
			}
		}

		begin = time.Now()

		err = tracing.DoInSpanWithErr(ctx, "compaction_block_upload", func(ctx context.Context) error {
			return block.Upload(ctx, cg.logger, cg.bkt, bdir, cg.hashFunc, objstore.WithUploadConcurrency(cg.blockFilesConcurrency))
		})
		if err != nil {
			return false, nil, retry(errors.Wrapf(err, "upload of %s failed", compID))
		}
		level.Info(cg.logger).Log("msg", "uploaded block", "result_block", compID, "duration", time.Since(begin), "duration_ms", time.Since(begin).Milliseconds())
		level.Info(cg.logger).Log("msg", "running post compaction callback", "result_block", compID)
		if err := compactionLifecycleCallback.PostCompactionCallback(ctx, cg.logger, cg, compID); err != nil {
			return false, nil, retry(errors.Wrapf(err, "failed to run post compaction callback for result block %s", compID))
		}
		level.Info(cg.logger).Log("msg", "finished running post compaction callback", "result_block", compID)
	}

	// Mark for deletion the blocks we just compacted from the group and bucket so they do not get included
	// into the next planning cycle.
	// Eventually the block we just uploaded should get synced into the group again (including sync-delay).
	for _, meta := range toCompact {
		if err := tracing.DoInSpanWithErr(ctx, "compaction_block_delete", func(ctx context.Context) error {
			return cg.deleteBlock(meta.ULID, filepath.Join(dir, meta.ULID.String()), blockDeletableChecker)
		}, opentracing.Tags{"block.id": meta.ULID}); err != nil {
			return false, nil, retry(errors.Wrapf(err, "mark old block for deletion from bucket"))
		}
		cg.groupGarbageCollectedBlocks.Inc()
	}

	level.Info(cg.logger).Log("msg", "finished compacting blocks", "duration", time.Since(groupCompactionBegin),
		"duration_ms", time.Since(groupCompactionBegin).Milliseconds(), "result_blocks", compIDStrs, "source_blocks", sourceBlockStr)
	return true, compIDs, nil
}

func (cg *Group) deleteBlock(id ulid.ULID, bdir string, blockDeletableChecker BlockDeletableChecker) error {
	if err := os.RemoveAll(bdir); err != nil {
		return errors.Wrapf(err, "remove old block dir %s", id)
	}

	if blockDeletableChecker.CanDelete(cg, id) {
		// Spawn a new context so we always mark a block for deletion in full on shutdown.
		delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		level.Info(cg.logger).Log("msg", "marking compacted block for deletion", "old_block", id)
		if err := block.MarkForDeletion(delCtx, cg.logger, cg.bkt, id, "source of compacted block", cg.blocksMarkedForDeletion); err != nil {
			return errors.Wrapf(err, "mark block %s for deletion from bucket", id)
		}
	}
	return nil
}

// BucketCompactor compacts blocks in a bucket.
type BucketCompactor struct {
	logger                         log.Logger
	sy                             *Syncer
	grouper                        Grouper
	comp                           Compactor
	planner                        Planner
	blockDeletableChecker          BlockDeletableChecker
	compactionLifecycleCallback    CompactionLifecycleCallback
	compactDir                     string
	bkt                            objstore.Bucket
	concurrency                    int
	skipBlocksWithOutOfOrderChunks bool
}

// NewBucketCompactor creates a new bucket compactor.
func NewBucketCompactor(
	logger log.Logger,
	sy *Syncer,
	grouper Grouper,
	planner Planner,
	comp Compactor,
	compactDir string,
	bkt objstore.Bucket,
	concurrency int,
	skipBlocksWithOutOfOrderChunks bool,
) (*BucketCompactor, error) {
	if concurrency <= 0 {
		return nil, errors.Errorf("invalid concurrency level (%d), concurrency level must be > 0", concurrency)
	}
	return NewBucketCompactorWithCheckerAndCallback(
		logger,
		sy,
		grouper,
		planner,
		comp,
		DefaultBlockDeletableChecker{},
		DefaultCompactionLifecycleCallback{},
		compactDir,
		bkt,
		concurrency,
		skipBlocksWithOutOfOrderChunks,
	)
}

func NewBucketCompactorWithCheckerAndCallback(
	logger log.Logger,
	sy *Syncer,
	grouper Grouper,
	planner Planner,
	comp Compactor,
	blockDeletableChecker BlockDeletableChecker,
	compactionLifecycleCallback CompactionLifecycleCallback,
	compactDir string,
	bkt objstore.Bucket,
	concurrency int,
	skipBlocksWithOutOfOrderChunks bool,
) (*BucketCompactor, error) {
	if concurrency <= 0 {
		return nil, errors.Errorf("invalid concurrency level (%d), concurrency level must be > 0", concurrency)
	}
	return &BucketCompactor{
		logger:                         logger,
		sy:                             sy,
		grouper:                        grouper,
		planner:                        planner,
		comp:                           comp,
		blockDeletableChecker:          blockDeletableChecker,
		compactionLifecycleCallback:    compactionLifecycleCallback,
		compactDir:                     compactDir,
		bkt:                            bkt,
		concurrency:                    concurrency,
		skipBlocksWithOutOfOrderChunks: skipBlocksWithOutOfOrderChunks,
	}, nil
}

// Compact runs compaction over bucket.
func (c *BucketCompactor) Compact(ctx context.Context) (rerr error) {
	defer func() {
		// Do not remove the compactDir if an error has occurred
		// because potentially on the next run we would not have to download
		// everything again.
		if rerr != nil {
			return
		}
		if err := os.RemoveAll(c.compactDir); err != nil {
			level.Error(c.logger).Log("msg", "failed to remove compaction work directory", "path", c.compactDir, "err", err)
		}
	}()

	// Loop over bucket and compact until there's no work left.
	for {
		var (
			wg                     sync.WaitGroup
			workCtx, workCtxCancel = context.WithCancel(ctx)
			groupChan              = make(chan *Group)
			errChan                = make(chan error, c.concurrency)
			finishedAllGroups      = true
			mtx                    sync.Mutex
		)
		defer workCtxCancel()

		// Set up workers who will compact the groups when the groups are ready.
		// They will compact available groups until they encounter an error, after which they will stop.
		for i := 0; i < c.concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for g := range groupChan {
					shouldRerunGroup, _, err := g.Compact(workCtx, c.compactDir, c.planner, c.comp, c.blockDeletableChecker, c.compactionLifecycleCallback)
					if err == nil {
						if shouldRerunGroup {
							mtx.Lock()
							finishedAllGroups = false
							mtx.Unlock()
						}
						continue
					}

					if IsIssue347Error(err) {
						if err := RepairIssue347(workCtx, c.logger, c.bkt, c.sy.metrics.BlocksMarkedForDeletion, err); err == nil {
							mtx.Lock()
							finishedAllGroups = false
							mtx.Unlock()
							continue
						}
					}
					// If block has out of order chunk and it has been configured to skip it,
					// then we can mark the block for no compaction so that the next compaction run
					// will skip it.
					if IsOutOfOrderChunkError(err) && c.skipBlocksWithOutOfOrderChunks {
						if err := block.MarkForNoCompact(
							ctx,
							c.logger,
							c.bkt,
							err.(OutOfOrderChunksError).id,
							metadata.OutOfOrderChunksNoCompactReason,
							"OutofOrderChunk: marking block with out-of-order series/chunks to as no compact to unblock compaction", g.blocksMarkedForNoCompact); err == nil {
							mtx.Lock()
							finishedAllGroups = false
							mtx.Unlock()
							continue
						}
					}
					errChan <- errors.Wrapf(err, "group %s", g.Key())
					return
				}
			}()
		}

		level.Info(c.logger).Log("msg", "start sync of metas")
		if err := c.sy.SyncMetas(ctx); err != nil {
			return errors.Wrap(err, "sync")
		}

		level.Info(c.logger).Log("msg", "start of GC")
		// Blocks that were compacted are garbage collected after each Compaction.
		// However if compactor crashes we need to resolve those on startup.
		if err := c.sy.GarbageCollect(ctx); err != nil {
			return errors.Wrap(err, "garbage")
		}

		groups, err := c.grouper.Groups(c.sy.Metas())
		if err != nil {
			return errors.Wrap(err, "build compaction groups")
		}

		ignoreDirs := []string{}
		for _, gr := range groups {
			for _, grID := range gr.IDs() {
				ignoreDirs = append(ignoreDirs, filepath.Join(gr.Key(), grID.String()))
			}
		}

		if err := runutil.DeleteAll(c.compactDir, ignoreDirs...); err != nil {
			level.Warn(c.logger).Log("msg", "failed deleting non-compaction group directories/files, some disk space usage might have leaked. Continuing", "err", err, "dir", c.compactDir)
		}

		level.Info(c.logger).Log("msg", "start of compactions")

		// Send all groups found during this pass to the compaction workers.
		var groupErrs errutil.MultiError
	groupLoop:
		for _, g := range groups {
			// Ignore groups with only one block because there is nothing to compact.
			if len(g.IDs()) == 1 {
				continue
			}
			select {
			case groupErr := <-errChan:
				groupErrs.Add(groupErr)
				break groupLoop
			case groupChan <- g:
			}
		}
		close(groupChan)
		wg.Wait()

		// Collect any other error reported by the workers, or any error reported
		// while we were waiting for the last batch of groups to run the compaction.
		close(errChan)
		for groupErr := range errChan {
			groupErrs.Add(groupErr)
		}

		workCtxCancel()
		if len(groupErrs) > 0 {
			return groupErrs.Err()
		}

		if finishedAllGroups {
			break
		}
	}
	level.Info(c.logger).Log("msg", "compaction iterations done")
	return nil
}

var _ block.MetadataFilter = &GatherNoCompactionMarkFilter{}

// GatherNoCompactionMarkFilter is a block.Fetcher filter that passes all metas. While doing it, it gathers all no-compact-mark.json markers.
// Not go routine safe.
// TODO(bwplotka): Add unit test.
type GatherNoCompactionMarkFilter struct {
	logger             log.Logger
	bkt                objstore.InstrumentedBucketReader
	noCompactMarkedMap map[ulid.ULID]*metadata.NoCompactMark
	concurrency        int
	mtx                sync.Mutex
}

// NewGatherNoCompactionMarkFilter creates GatherNoCompactionMarkFilter.
func NewGatherNoCompactionMarkFilter(logger log.Logger, bkt objstore.InstrumentedBucketReader, concurrency int) *GatherNoCompactionMarkFilter {
	return &GatherNoCompactionMarkFilter{
		logger:      logger,
		bkt:         bkt,
		concurrency: concurrency,
	}
}

// NoCompactMarkedBlocks returns block ids that were marked for no compaction.
func (f *GatherNoCompactionMarkFilter) NoCompactMarkedBlocks() map[ulid.ULID]*metadata.NoCompactMark {
	f.mtx.Lock()
	copiedNoCompactMarked := make(map[ulid.ULID]*metadata.NoCompactMark, len(f.noCompactMarkedMap))
	for k, v := range f.noCompactMarkedMap {
		copiedNoCompactMarked[k] = v
	}
	f.mtx.Unlock()

	return copiedNoCompactMarked
}

// Filter passes all metas, while gathering no compact markers.
func (f *GatherNoCompactionMarkFilter) Filter(ctx context.Context, metas map[ulid.ULID]*metadata.Meta, synced block.GaugeVec, modified block.GaugeVec) error {
	var localNoCompactMapMtx sync.Mutex

	noCompactMarkedMap := make(map[ulid.ULID]*metadata.NoCompactMark)

	// Make a copy of block IDs to check, in order to avoid concurrency issues
	// between the scheduler and workers.
	blockIDs := make([]ulid.ULID, 0, len(metas))
	for id := range metas {
		blockIDs = append(blockIDs, id)
	}

	var (
		eg errgroup.Group
		ch = make(chan ulid.ULID, f.concurrency)
	)

	for i := 0; i < f.concurrency; i++ {
		eg.Go(func() error {
			var lastErr error
			for id := range ch {
				m := &metadata.NoCompactMark{}
				// TODO(bwplotka): Hook up bucket cache here + reset API so we don't introduce API calls .
				if err := metadata.ReadMarker(ctx, f.logger, f.bkt, id.String(), m); err != nil {
					if errors.Cause(err) == metadata.ErrorMarkerNotFound {
						continue
					}
					if errors.Cause(err) == metadata.ErrorUnmarshalMarker {
						level.Warn(f.logger).Log("msg", "found partial no-compact-mark.json; if we will see it happening often for the same block, consider manually deleting no-compact-mark.json from the object storage", "block", id, "err", err)
						continue
					}
					// Remember the last error and continue draining the channel.
					lastErr = err
					continue
				}

				localNoCompactMapMtx.Lock()
				noCompactMarkedMap[id] = m
				localNoCompactMapMtx.Unlock()
				synced.WithLabelValues(block.MarkedForNoCompactionMeta).Inc()
			}

			return lastErr
		})
	}

	// Workers scheduled, distribute blocks.
	eg.Go(func() error {
		defer close(ch)

		for _, id := range blockIDs {
			select {
			case ch <- id:
				// Nothing to do.
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return errors.Wrap(err, "filter blocks marked for no compaction")
	}

	f.mtx.Lock()
	f.noCompactMarkedMap = noCompactMarkedMap
	f.mtx.Unlock()

	return nil
}
