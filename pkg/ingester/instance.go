package ingester

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/weaveworks/common/httpgrpc"

	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ingester/index"
	cutil "github.com/cortexproject/cortex/pkg/util"

	"github.com/grafana/loki/pkg/chunkenc"
	"github.com/grafana/loki/pkg/helpers"
	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/loghttp"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logql/stats"
	"github.com/grafana/loki/pkg/util"
)

const queryBatchSize = 128

// Errors returned on Query.
var (
	ErrStreamMissing = errors.New("Stream missing")
)

var (
	memoryStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "loki",
		Name:      "ingester_memory_streams",
		Help:      "The total number of streams in memory.",
	})
	streamsCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loki",
		Name:      "ingester_streams_created_total",
		Help:      "The total number of streams created per tenant.",
	}, []string{"tenant"})
	streamsRemovedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loki",
		Name:      "ingester_streams_removed_total",
		Help:      "The total number of streams removed per tenant.",
	}, []string{"tenant"})
)

type instance struct {
	cfg        *Config
	streamsMtx sync.RWMutex
	streams    map[model.Fingerprint]*stream // we use 'mapped' fingerprints here.
	index      *index.InvertedIndex
	mapper     *fpMapper // using of mapper needs streamsMtx because it calls back

	instanceID string

	streamsCreatedTotal prometheus.Counter
	streamsRemovedTotal prometheus.Counter

	tailers   map[uint32]*tailer
	tailerMtx sync.RWMutex

	limiter *Limiter
	factory func() chunkenc.Chunk

	// sync
	syncPeriod  time.Duration
	syncMinUtil float64
}

func newInstance(cfg *Config, instanceID string, factory func() chunkenc.Chunk, limiter *Limiter, syncPeriod time.Duration, syncMinUtil float64) *instance {
	i := &instance{
		cfg:        cfg,
		streams:    map[model.Fingerprint]*stream{},
		index:      index.New(),
		instanceID: instanceID,

		streamsCreatedTotal: streamsCreatedTotal.WithLabelValues(instanceID),
		streamsRemovedTotal: streamsRemovedTotal.WithLabelValues(instanceID),

		factory: factory,
		tailers: map[uint32]*tailer{},
		limiter: limiter,

		syncPeriod:  syncPeriod,
		syncMinUtil: syncMinUtil,
	}
	i.mapper = newFPMapper(i.getLabelsFromFingerprint)
	return i
}

// consumeChunk manually adds a chunk that was received during ingester chunk
// transfer.
func (i *instance) consumeChunk(ctx context.Context, labels []client.LabelAdapter, chunk *logproto.Chunk) error {
	i.streamsMtx.Lock()
	defer i.streamsMtx.Unlock()

	rawFp := client.FastFingerprint(labels)
	fp := i.mapper.mapFP(rawFp, labels)

	stream, ok := i.streams[fp]
	if !ok {
		sortedLabels := i.index.Add(labels, fp)
		stream = newStream(i.cfg, fp, sortedLabels, i.factory)
		i.streams[fp] = stream
		i.streamsCreatedTotal.Inc()
		memoryStreams.Inc()
		i.addTailersToNewStream(stream)
	}

	err := stream.consumeChunk(ctx, chunk)
	if err == nil {
		memoryChunks.Inc()
	}

	return err
}

func (i *instance) Push(ctx context.Context, req *logproto.PushRequest) error {
	i.streamsMtx.Lock()
	defer i.streamsMtx.Unlock()

	var appendErr error
	for _, s := range req.Streams {
		labels, err := util.ToClientLabels(s.Labels)
		if err != nil {
			appendErr = err
			continue
		}

		stream, err := i.getOrCreateStream(labels)
		if err != nil {
			appendErr = err
			continue
		}

		prevNumChunks := len(stream.chunks)
		if err := stream.Push(ctx, s.Entries, i.syncPeriod, i.syncMinUtil); err != nil {
			appendErr = err
			continue
		}

		memoryChunks.Add(float64(len(stream.chunks) - prevNumChunks))
	}

	return appendErr
}

func (i *instance) getOrCreateStream(labels []client.LabelAdapter) (*stream, error) {
	rawFp := client.FastFingerprint(labels)
	fp := i.mapper.mapFP(rawFp, labels)

	stream, ok := i.streams[fp]
	if ok {
		return stream, nil
	}

	err := i.limiter.AssertMaxStreamsPerUser(i.instanceID, len(i.streams))
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, err.Error())
	}

	sortedLabels := i.index.Add(labels, fp)
	stream = newStream(i.cfg, fp, sortedLabels, i.factory)
	i.streams[fp] = stream
	memoryStreams.Inc()
	i.streamsCreatedTotal.Inc()
	i.addTailersToNewStream(stream)

	return stream, nil
}

// Return labels associated with given fingerprint. Used by fingerprint mapper. Must hold streamsMtx.
func (i *instance) getLabelsFromFingerprint(fp model.Fingerprint) labels.Labels {
	s := i.streams[fp]
	if s == nil {
		return nil
	}
	return s.labels
}

func (i *instance) Query(req *logproto.QueryRequest, queryServer logproto.Querier_QueryServer) error {
	// initialize stats collection for ingester queries and set grpc trailer with stats.
	ctx := stats.NewContext(queryServer.Context())
	defer stats.SendAsTrailer(ctx, queryServer)

	expr, err := (logql.SelectParams{QueryRequest: req}).LogSelector()
	if err != nil {
		return err
	}
	filter, err := expr.Filter()
	if err != nil {
		return err
	}

	ingStats := stats.GetIngesterData(ctx)
	var iters []iter.EntryIterator
	err = i.forMatchingStreams(
		expr.Matchers(),
		func(stream *stream) error {
			ingStats.TotalChunksMatched += int64(len(stream.chunks))
			iter, err := stream.Iterator(ctx, req.Start, req.End, req.Direction, filter)
			if err != nil {
				return err
			}
			iters = append(iters, iter)
			return nil
		},
	)
	if err != nil {
		return err
	}

	iter := iter.NewHeapIterator(ctx, iters, req.Direction)
	defer helpers.LogError("closing iterator", iter.Close)

	return sendBatches(ctx, iter, queryServer, req.Limit)
}

func (i *instance) Label(_ context.Context, req *logproto.LabelRequest) (*logproto.LabelResponse, error) {
	var labels []string
	if req.Values {
		values := i.index.LabelValues(req.Name)
		labels = make([]string, len(values))
		for i := 0; i < len(values); i++ {
			labels[i] = values[i]
		}
	} else {
		names := i.index.LabelNames()
		labels = make([]string, len(names))
		for i := 0; i < len(names); i++ {
			labels[i] = names[i]
		}
	}
	return &logproto.LabelResponse{
		Values: labels,
	}, nil
}

func (i *instance) Series(_ context.Context, req *logproto.SeriesRequest) (*logproto.SeriesResponse, error) {
	groups, err := loghttp.Match(req.GetGroups())
	if err != nil {
		return nil, err
	}

	dedupedSeries := make(map[uint64]logproto.SeriesIdentifier)
	for _, matchers := range groups {
		err = i.forMatchingStreams(matchers, func(stream *stream) error {
			// exit early when this stream was added by an earlier group
			key := stream.labels.Hash()
			if _, found := dedupedSeries[key]; found {
				return nil
			}

			dedupedSeries[key] = logproto.SeriesIdentifier{
				Labels: stream.labels.Map(),
			}
			return nil
		})

		if err != nil {
			return nil, err
		}
	}
	series := make([]logproto.SeriesIdentifier, 0, len(dedupedSeries))
	for _, v := range dedupedSeries {
		series = append(series, v)

	}
	return &logproto.SeriesResponse{Series: series}, nil
}

// forMatchingStreams will execute a function for each stream that satisfies a set of requirements (time range, matchers, etc).
// It uses a function in order to enable generic stream acces without accidentally leaking streams under the mutex.
func (i *instance) forMatchingStreams(
	matchers []*labels.Matcher,
	fn func(*stream) error,
) error {
	i.streamsMtx.RLock()
	defer i.streamsMtx.RUnlock()

	filters, matchers := cutil.SplitFiltersAndMatchers(matchers)
	ids := i.index.Lookup(matchers)

outer:
	for _, streamID := range ids {
		stream, ok := i.streams[streamID]
		if !ok {
			return ErrStreamMissing
		}
		for _, filter := range filters {
			if !filter.Matches(stream.labels.Get(filter.Name)) {
				continue outer
			}
		}

		err := fn(stream)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) addNewTailer(t *tailer) {
	i.streamsMtx.RLock()
	for _, stream := range i.streams {
		if stream.matchesTailer(t) {
			stream.addTailer(t)
		}
	}
	i.streamsMtx.RUnlock()

	i.tailerMtx.Lock()
	defer i.tailerMtx.Unlock()
	i.tailers[t.getID()] = t
}

func (i *instance) addTailersToNewStream(stream *stream) {
	i.tailerMtx.RLock()
	defer i.tailerMtx.RUnlock()

	for _, t := range i.tailers {
		// we don't want to watch streams for closed tailers.
		// When a new tail request comes in we will clean references to closed tailers
		if t.isClosed() {
			continue
		}

		if stream.matchesTailer(t) {
			stream.addTailer(t)
		}
	}
}

func (i *instance) checkClosedTailers() {
	closedTailers := []uint32{}

	i.tailerMtx.RLock()
	for _, t := range i.tailers {
		if t.isClosed() {
			closedTailers = append(closedTailers, t.getID())
			continue
		}
	}
	i.tailerMtx.RUnlock()

	if len(closedTailers) != 0 {
		i.tailerMtx.Lock()
		defer i.tailerMtx.Unlock()
		for _, closedTailer := range closedTailers {
			delete(i.tailers, closedTailer)
		}
	}
}

func (i *instance) closeTailers() {
	i.tailerMtx.Lock()
	defer i.tailerMtx.Unlock()
	for _, t := range i.tailers {
		t.close()
	}
}

func (i *instance) openTailersCount() uint32 {
	i.checkClosedTailers()

	i.tailerMtx.RLock()
	defer i.tailerMtx.RUnlock()

	return uint32(len(i.tailers))
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func sendBatches(ctx context.Context, i iter.EntryIterator, queryServer logproto.Querier_QueryServer, limit uint32) error {
	ingStats := stats.GetIngesterData(ctx)
	if limit == 0 {
		// send all batches.
		for !isDone(ctx) {
			batch, size, err := iter.ReadBatch(i, queryBatchSize)
			if err != nil {
				return err
			}
			if len(batch.Streams) == 0 {
				return nil
			}

			if err := queryServer.Send(batch); err != nil {
				return err
			}
			ingStats.TotalLinesSent += int64(size)
			ingStats.TotalBatches++
		}
		return nil
	}
	// send until the limit is reached.
	sent := uint32(0)
	for sent < limit && !isDone(queryServer.Context()) {
		batch, batchSize, err := iter.ReadBatch(i, helpers.MinUint32(queryBatchSize, limit-sent))
		if err != nil {
			return err
		}
		sent += batchSize

		if len(batch.Streams) == 0 {
			return nil
		}

		if err := queryServer.Send(batch); err != nil {
			return err
		}
		ingStats.TotalLinesSent += int64(batchSize)
		ingStats.TotalBatches++
	}
	return nil
}
