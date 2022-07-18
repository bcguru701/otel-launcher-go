// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package syncstate

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/aggregator/aggregation"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/aggregator/gauge"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/aggregator/histogram"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/aggregator/sum"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/data"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/internal/pipeline"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/internal/test"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/internal/viewstate"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/number"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/sdkinstrument"
	"github.com/lightstep/otel-launcher-go/lightstep/sdk/metric/view"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/sdk/instrumentation"
)

var (
	endTime    = time.Unix(100, 0)
	middleTime = time.Unix(99, 0)
	startTime  = time.Unix(98, 0)

	testSequence = data.Sequence{
		Start: startTime,
		Last:  middleTime,
		Now:   endTime,
	}
)

func deltaUpdate[N number.Any](old, new N) N {
	return old + new
}

func cumulativeUpdate[N number.Any](_, new N) N {
	return new
}

const testAttr = attribute.Key("key")

var (
	deltaSelector = view.WithDefaultAggregationTemporalitySelector(func(_ sdkinstrument.Kind) aggregation.Temporality {
		return aggregation.DeltaTemporality
	})

	cumulativeSelector = view.WithDefaultAggregationTemporalitySelector(func(_ sdkinstrument.Kind) aggregation.Temporality {
		return aggregation.CumulativeTemporality
	})

	keyFilter = view.WithClause(
		view.WithKeys([]attribute.Key{}),
	)
)

func TestSyncStateDeltaConcurrencyInt(t *testing.T) {
	testSyncStateConcurrency[int64, number.Int64Traits](t, deltaUpdate[int64], deltaSelector)
}

func TestSyncStateCumulativeConcurrencyInt(t *testing.T) {
	testSyncStateConcurrency[int64, number.Int64Traits](t, cumulativeUpdate[int64], cumulativeSelector)
}

func TestSyncStateCumulativeConcurrencyIntFiltered(t *testing.T) {
	testSyncStateConcurrency[int64, number.Int64Traits](t, cumulativeUpdate[int64], cumulativeSelector, keyFilter)
}

func TestSyncStateDeltaConcurrencyFloat(t *testing.T) {
	testSyncStateConcurrency[float64, number.Float64Traits](t, deltaUpdate[float64], deltaSelector)
}

func TestSyncStateCumulativeConcurrencyFloat(t *testing.T) {
	testSyncStateConcurrency[float64, number.Float64Traits](t, cumulativeUpdate[float64], cumulativeSelector)
}

func TestSyncStateCumulativeConcurrencyFloatFiltered(t *testing.T) {
	testSyncStateConcurrency[float64, number.Float64Traits](t, cumulativeUpdate[float64], cumulativeSelector, keyFilter)
}

func testSyncStateConcurrency[N number.Any, Traits number.Traits[N]](t *testing.T, update func(old, new N) N, vopts ...view.Option) {
	const (
		numReaders  = 2
		numRoutines = 10
		numAttrs    = 10
		numUpdates  = 1e5
	)

	var traits Traits
	var writers sync.WaitGroup
	var readers sync.WaitGroup

	readers.Add(numReaders)
	writers.Add(numRoutines)

	lib := instrumentation.Library{
		Name: "testlib",
	}
	vcs := make([]*viewstate.Compiler, numReaders)
	for vci := range vcs {
		vcs[vci] = viewstate.New(lib, view.New("test", vopts...))
	}
	attrs := make([]attribute.KeyValue, numAttrs)
	for i := range attrs {
		attrs[i] = testAttr.Int(i)
	}

	desc := test.Descriptor("tester", sdkinstrument.SyncCounter, traits.Kind())

	pipes := make(pipeline.Register[viewstate.Instrument], numReaders)
	for vci := range vcs {
		pipes[vci], _ = vcs[vci].Compile(desc)
	}

	inst := NewInstrument(desc, nil, pipes)
	require.NotNil(t, inst)

	cntr := NewCounter[N, Traits](inst)
	require.NotNil(t, cntr)

	ctx, cancel := context.WithCancel(context.Background())

	partialCounts := make([]map[attribute.Set]N, numReaders)

	for vci := range vcs {
		partialCounts[vci] = map[attribute.Set]N{}
	}

	// Reader loops
	for vci := range vcs {
		go func(vci int, partial map[attribute.Set]N, vc *viewstate.Compiler) {
			defer readers.Done()

			// scope will be reused by this reader
			var scope data.Scope
			seq := data.Sequence{
				Start: time.Now(),
			}
			seq.Now = seq.Start

			collect := func() {
				seq.Last = seq.Now
				seq.Now = time.Now()

				inst.SnapshotAndProcess()

				scope.Reset()

				vc.Collectors()[0].Collect(seq, &scope.Instruments)

				for _, pt := range scope.Instruments[0].Points {
					partial[pt.Attributes] = update(partial[pt.Attributes], traits.FromNumber(pt.Aggregation.(*sum.State[N, Traits, sum.Monotonic]).Sum()))
				}
			}

			for {
				select {
				case <-ctx.Done():
					collect()
					return
				default:
					collect()
				}
			}
		}(vci, partialCounts[vci], vcs[vci])
	}

	// Writer loops
	for i := 0; i < numRoutines; i++ {
		go func() {
			defer writers.Done()
			rnd := rand.New(rand.NewSource(rand.Int63()))

			for j := 0; j < numUpdates/numRoutines; j++ {
				cntr.Add(ctx, 1, attrs[rnd.Intn(len(attrs))])
			}
		}()
	}

	writers.Wait()
	cancel()
	readers.Wait()

	for vci := range vcs {
		var sum N
		for _, count := range partialCounts[vci] {
			sum += count
		}
		require.Equal(t, N(numUpdates), sum, "vci==%d", vci)
	}
}

func TestSyncStatePartialNoopInstrument(t *testing.T) {
	ctx := context.Background()
	vopts := []view.Option{
		view.WithClause(
			view.MatchInstrumentName("dropme"),
			view.WithAggregation(aggregation.DropKind),
		),
	}
	lib := instrumentation.Library{
		Name: "testlib",
	}
	vcs := make([]*viewstate.Compiler, 2)
	vcs[0] = viewstate.New(lib, view.New("dropper", vopts...))
	vcs[1] = viewstate.New(lib, view.New("keeper"))

	desc := test.Descriptor("dropme", sdkinstrument.SyncHistogram, number.Float64Kind)

	pipes := make(pipeline.Register[viewstate.Instrument], 2)
	pipes[0], _ = vcs[0].Compile(desc)
	pipes[1], _ = vcs[1].Compile(desc)

	require.Nil(t, pipes[0])
	require.NotNil(t, pipes[1])

	inst := NewInstrument(desc, nil, pipes)
	require.NotNil(t, inst)

	hist := NewHistogram[float64, number.Float64Traits](inst)
	require.NotNil(t, hist)

	hist.Record(ctx, 1)
	hist.Record(ctx, 2)
	hist.Record(ctx, 3)

	inst.SnapshotAndProcess()

	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
	)

	// Note: Create a merged histogram that is exactly equal to
	// the one we expect.  Merging creates a slightly different
	// struct, despite identical value, so we merge to create the
	// expected value:
	expectHist := histogram.NewFloat64(histogram.NewConfig())
	mergeIn := histogram.NewFloat64(histogram.NewConfig(), 1, 2, 3)
	var methods histogram.Float64Methods
	methods.Copy(mergeIn, expectHist)

	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[1].Collectors(),
			testSequence,
		),
		test.Instrument(
			desc,
			test.Point(startTime, endTime,
				expectHist,
				aggregation.CumulativeTemporality,
			),
		),
	)
}

func TestSyncStateFullNoopInstrument(t *testing.T) {
	ctx := context.Background()
	vopts := []view.Option{
		view.WithClause(
			view.MatchInstrumentName("dropme"),
			view.WithAggregation(aggregation.DropKind),
		),
	}
	lib := instrumentation.Library{
		Name: "testlib",
	}
	vcs := make([]*viewstate.Compiler, 2)
	vcs[0] = viewstate.New(lib, view.New("dropper", vopts...))
	vcs[1] = viewstate.New(lib, view.New("keeper", vopts...))

	desc := test.Descriptor("dropme", sdkinstrument.SyncHistogram, number.Float64Kind)

	pipes := make(pipeline.Register[viewstate.Instrument], 2)
	pipes[0], _ = vcs[0].Compile(desc)
	pipes[1], _ = vcs[1].Compile(desc)

	require.Nil(t, pipes[0])
	require.Nil(t, pipes[1])

	inst := NewInstrument(desc, nil, pipes)
	require.Nil(t, inst)

	hist := NewHistogram[float64, number.Float64Traits](inst)
	require.NotNil(t, hist)

	hist.Record(ctx, 1)
	hist.Record(ctx, 2)
	hist.Record(ctx, 3)

	// There's no instrument, nothing to Snapshot
	require.Equal(t, 0, len(vcs[0].Collectors()))
	require.Equal(t, 0, len(vcs[1].Collectors()))
}

func TestOutOfRangeValues(t *testing.T) {
	for _, desc := range []sdkinstrument.Descriptor{
		test.Descriptor("cf", sdkinstrument.SyncCounter, number.Float64Kind),
		test.Descriptor("uf", sdkinstrument.SyncUpDownCounter, number.Float64Kind),
		test.Descriptor("hf", sdkinstrument.SyncHistogram, number.Float64Kind),
		test.Descriptor("ci", sdkinstrument.SyncCounter, number.Int64Kind),
		test.Descriptor("ui", sdkinstrument.SyncUpDownCounter, number.Int64Kind),
		test.Descriptor("hi", sdkinstrument.SyncHistogram, number.Int64Kind),
	} {
		ctx := context.Background()
		lib := instrumentation.Library{
			Name: "testlib",
		}
		vcs := make([]*viewstate.Compiler, 1)
		vcs[0] = viewstate.New(lib, view.New("test"))

		pipes := make(pipeline.Register[viewstate.Instrument], 1)
		pipes[0], _ = vcs[0].Compile(desc)

		inst := NewInstrument(desc, nil, pipes)
		require.NotNil(t, inst)

		var negOne aggregation.Aggregation

		if desc.NumberKind == number.Float64Kind {
			cntr := NewCounter[float64, number.Float64Traits](inst)

			cntr.Add(ctx, -1)
			cntr.Add(ctx, math.NaN())
			cntr.Add(ctx, math.Inf(+1))
			cntr.Add(ctx, math.Inf(-1))
			negOne = sum.NewNonMonotonicFloat64(-1)
		} else {
			cntr := NewCounter[int64, number.Int64Traits](inst)

			cntr.Add(ctx, -1)
			negOne = sum.NewNonMonotonicInt64(-1)
		}

		inst.SnapshotAndProcess()

		var expectPoints []data.Point

		if desc.Kind == sdkinstrument.SyncUpDownCounter {
			expectPoints = append(expectPoints, test.Point(
				startTime, endTime,
				negOne,
				aggregation.CumulativeTemporality,
			))
		}

		test.RequireEqualMetrics(
			t,
			test.CollectScope(
				t,
				vcs[0].Collectors(),
				testSequence,
			),
			test.Instrument(
				desc,
				expectPoints...,
			),
		)
	}
}

func TestSyncGaugeDeltaInstrument(t *testing.T) {
	ctx := context.Background()
	lib := instrumentation.Library{
		Name: "testlib",
	}
	vcs := make([]*viewstate.Compiler, 2)
	vcs[0] = viewstate.New(lib, view.New(
		"test",
		deltaSelector,
		view.WithClause(
			view.WithKeys([]attribute.Key{"A", "C"}),
		),
	))

	indesc := test.Descriptor(
		"syncgauge",
		sdkinstrument.SyncUpDownCounter,
		number.Float64Kind,
		instrument.WithDescription(`{
  "aggregation": "gauge",
  "description": "incredible"
}`))

	outdesc := test.Descriptor(
		"syncgauge",
		sdkinstrument.SyncUpDownCounter,
		number.Float64Kind,
		instrument.WithDescription("incredible"),
	)

	pipes := make(pipeline.Register[viewstate.Instrument], 1)
	pipes[0], _ = vcs[0].Compile(indesc)

	require.NotNil(t, pipes[0])

	inst := NewInstrument(indesc, nil, pipes)
	require.NotNil(t, inst)

	sg := NewCounter[float64, number.Float64Traits](inst)
	require.NotNil(t, sg)

	sg.Add(ctx, 1)
	sg.Add(ctx, 2)
	sg.Add(ctx, 3)

	inst.SnapshotAndProcess()
	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
		test.Instrument(
			outdesc,
			test.Point(middleTime, endTime,
				gauge.NewFloat64(3),
				aggregation.DeltaTemporality,
			),
		),
	)

	// If not set, it disappears.
	inst.SnapshotAndProcess()
	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
		test.Instrument(
			outdesc,
		),
	)

	// Set again
	sg.Add(ctx, 172)
	sg.Add(ctx, 175)

	inst.SnapshotAndProcess()
	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
		test.Instrument(
			outdesc,
			test.Point(middleTime, endTime,
				gauge.NewFloat64(175),
				aggregation.DeltaTemporality,
			),
		),
	)

	// Set different attribute sets, leave the first (empty set) unused.
	sg.Add(ctx, 1333, attribute.String("A", "B"))
	sg.Add(ctx, 1337, attribute.String("C", "D"))

	inst.SnapshotAndProcess()
	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
		test.Instrument(
			outdesc,
			test.Point(middleTime, endTime,
				gauge.NewFloat64(1333),
				aggregation.DeltaTemporality,
				attribute.String("A", "B"),
			),
			test.Point(middleTime, endTime,
				gauge.NewFloat64(1337),
				aggregation.DeltaTemporality,
				attribute.String("C", "D"),
			),
		),
	)

	// Test the filters.  Last value should win due to the Gauge
	// sequence number (as opposed to random choice, which would
	// happen naturally b/c of map iteration).
	for i := 0; i < 1000; i++ {
		sg.Add(ctx, float64(i), attribute.Int("ignored", i), attribute.String("A", "B"))
	}
	for i := 1000; i > 0; i-- {
		sg.Add(ctx, float64(i), attribute.Int("ignored", i), attribute.String("C", "D"))
	}

	inst.SnapshotAndProcess()
	test.RequireEqualMetrics(
		t,
		test.CollectScope(
			t,
			vcs[0].Collectors(),
			testSequence,
		),
		test.Instrument(
			outdesc,
			test.Point(middleTime, endTime,
				gauge.NewFloat64(999),
				aggregation.DeltaTemporality,
				attribute.String("A", "B"),
			),
			test.Point(middleTime, endTime,
				gauge.NewFloat64(1),
				aggregation.DeltaTemporality,
				attribute.String("C", "D"),
			),
		),
	)
}
