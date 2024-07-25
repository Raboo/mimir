// SPDX-License-Identifier: AGPL-3.0-only

package queue

import (
	"container/list"
	"context"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"
)

const querierForgetDelay = 0
const maxOutStandingPerTenant = 1000000

func newTQA() *tenantQuerierAssignments {
	currentQuerier := QuerierID("")
	return &tenantQuerierAssignments{
		queriersByID:       map[QuerierID]*querierConn{},
		querierIDsSorted:   nil,
		querierForgetDelay: querierForgetDelay,
		tenantIDOrder:      nil,
		tenantsByID:        map[TenantID]*queueTenant{},
		tenantQuerierIDs:   map[TenantID]map[QuerierID]struct{}{},
		tenantNodes:        map[string][]*Node{},
		currentQuerier:     currentQuerier,
		tenantOrderIndex:   localQueueIndex,
	}
}

func newBenchmarkRequestQueue(
	queryComponentUtilization *QueryComponentUtilization,
	tenantQuerierAssignments *tenantQuerierAssignments,
	tree Tree,
	prioritizeQueryComponents bool,
) (*RequestQueue, error) {

	q := &RequestQueue{
		// settings
		log:                              log.NewNopLogger(),
		maxOutstandingPerTenant:          maxOutStandingPerTenant,
		additionalQueueDimensionsEnabled: true,
		forgetDelay:                      time.Duration(querierForgetDelay),

		// metrics for reporting
		connectedQuerierWorkers: atomic.NewInt64(0),
		queueLength:             promauto.With(nil).NewGaugeVec(prometheus.GaugeOpts{}, []string{"user"}),
		discardedRequests:       promauto.With(nil).NewCounterVec(prometheus.CounterOpts{}, []string{"user"}),
		enqueueDuration:         promauto.With(nil).NewHistogram(prometheus.HistogramOpts{}),

		// channels must not be buffered so that we can detect when dispatcherLoop() has finished.
		stopRequested: make(chan struct{}),
		stopCompleted: make(chan struct{}),

		requestsToEnqueue:             make(chan requestToEnqueue),
		requestsSent:                  make(chan *SchedulerRequest),
		requestsCompleted:             make(chan *SchedulerRequest),
		querierOperations:             make(chan querierOperation),
		waitingQuerierConns:           make(chan *waitingQuerierConn),
		waitingQuerierConnsToDispatch: list.New(),

		QueryComponentUtilization: queryComponentUtilization,
		queueBroker: &queueBroker{
			tree:                             tree,
			tenantQuerierAssignments:         tenantQuerierAssignments,
			maxTenantQueueSize:               maxOutStandingPerTenant,
			additionalQueueDimensionsEnabled: true,
			prioritizeQueryComponents:        prioritizeQueryComponents,
		},
	}
	q.Service = services.NewBasicService(q.starting, q.running, q.stop).WithName("request queue")
	return q, nil
}

func weightedRandAdditionalQueueDimension(dimensionWeights map[string]float64) string {
	totalWeight := float64(0)
	for _, dimensionWeight := range dimensionWeights {
		totalWeight += dimensionWeight
	}
	roundTotalWeight := math.Round(totalWeight*10) / 10

	if roundTotalWeight != 1.0 {
		panic("dimension weights must sum to 1.0")
	}

	randInt := rand.Float64()

	sum := float64(0)
	for dimension, dimensionWeight := range dimensionWeights {
		sum += dimensionWeight
		if randInt < sum {
			return dimension
		}
	}

	panic("no dimension selected")
}

func makeWeightedRandAdditionalQueueDimensionFunc(
	tenantDimensionWeights map[string]map[string]float64,
) func(tenantID string) []string {
	return func(tenantID string) []string {
		return []string{weightedRandAdditionalQueueDimension(tenantDimensionWeights[tenantID])}
	}
}

func makeQueueProducerGroup(
	queue *RequestQueue,
	maxQueriersPerTenant int,
	totalRequests int,
	numProducers int,
	numTenants int,
	queueDimensionFunc func(string) []string,
) (chan struct{}, *errgroup.Group) {
	startProducersChan := make(chan struct{})
	producersErrGroup, _ := errgroup.WithContext(context.Background())

	runProducer := runQueueProducerIters(
		queue, maxQueriersPerTenant, totalRequests, numProducers, numTenants, startProducersChan, queueDimensionFunc,
	)
	for producerIdx := 0; producerIdx < numProducers; producerIdx++ {
		producerIdx := producerIdx
		producersErrGroup.Go(func() error {
			return runProducer(producerIdx)
		})
	}
	return startProducersChan, producersErrGroup
}

func makeQueueConsumeFunc(
	queue *RequestQueue,
	slowConsumerQueueDimension string,
	slowConsumerLatency time.Duration,
	normalConsumerLatency time.Duration,
	report *testScenarioQueueDurationReport,
) consumeRequest {
	return func(request Request) error {
		schedulerRequest := request.(*SchedulerRequest)
		queryComponent := schedulerRequest.ExpectedQueryComponentName()
		if queryComponent == ingesterAndStoreGatewayQueueDimension {
			queryComponent = storeGatewayQueueDimension
		}
		report.Observe(schedulerRequest.UserID, queryComponent, time.Since(schedulerRequest.EnqueueTime).Seconds())

		queue.QueryComponentUtilization.MarkRequestSent(schedulerRequest)
		if queryComponent == slowConsumerQueueDimension {
			time.Sleep(slowConsumerLatency)
		} else {
			time.Sleep(normalConsumerLatency)
		}

		queue.QueryComponentUtilization.MarkRequestCompleted(schedulerRequest)
		return nil
	}
}

func makeQueueConsumerGroup(queue *RequestQueue, totalRequests int, numConsumers int, consumeFunc consumeRequest) (*errgroup.Group, chan struct{}) {
	queueConsumerErrGroup, ctx := errgroup.WithContext(context.Background())
	consumedRequestsCounter := make(chan struct{}, totalRequests)
	startConsumersChan := make(chan struct{})
	stopConsumersChan := make(chan struct{})
	runConsumer := runQueueConsumerUntilEmpty(ctx, totalRequests, queue, consumeFunc, consumedRequestsCounter, startConsumersChan, stopConsumersChan)

	for consumerIdx := 0; consumerIdx < numConsumers; consumerIdx++ {
		consumerIdx := consumerIdx
		queueConsumerErrGroup.Go(func() error {
			return runConsumer(consumerIdx)
		})
	}
	return queueConsumerErrGroup, startConsumersChan
}

type testScenarioQueueDurationReport struct {
	componentUtilizationReservedCapacity float64

	mu                                      sync.Mutex
	tenantIDQueueDurationObservations       map[string][]float64
	queryComponentQueueDurationObservations map[string][]float64
}

func (report *testScenarioQueueDurationReport) Observe(tenantID, queryComponent string, queueDuration float64) {
	report.mu.Lock()
	defer report.mu.Unlock()

	report.tenantIDQueueDurationObservations[tenantID] = append(
		report.tenantIDQueueDurationObservations[tenantID], queueDuration,
	)
	report.queryComponentQueueDurationObservations[queryComponent] = append(
		report.queryComponentQueueDurationObservations[queryComponent], queueDuration,
	)
}

func (report *testScenarioQueueDurationReport) String() string {
	var tenantIDs []string
	for tenantID := range report.tenantIDQueueDurationObservations {
		tenantIDs = append(tenantIDs, tenantID)
	}
	slices.Sort(tenantIDs)
	var queryComponents []string
	for queryComponent := range report.queryComponentQueueDurationObservations {
		queryComponents = append(queryComponents, queryComponent)
	}
	slices.Sort(queryComponents)

	// punting this as we only have tested for one tenant so far
	//var tenantReports []string
	//for _, tenantID := range tenantIDs {
	//	tenantIDMeanDuration := mean(report.tenantIDQueueDurationObservations[tenantID])
	//	tenantIDStdDev := stddev(report.tenantIDQueueDurationObservations[tenantID], tenantIDMeanDuration)
	//	tenantReports = append(tenantReports, fmt.Sprintf("tenant %s: mean: %.2f stddev: %.2f", tenantID, tenantIDMeanDuration, tenantIDStdDev))
	//}

	var queryComponentReports []string
	for _, queryComponent := range queryComponents {
		//percentile95 := percentile(report.queryComponentQueueDurationObservations[queryComponent], 0.95)
		meanDur := mean(report.queryComponentQueueDurationObservations[queryComponent])
		stdDevDur := stddev(report.queryComponentQueueDurationObservations[queryComponent], meanDur)
		queryComponentReports = append(
			queryComponentReports,
			fmt.Sprintf("%s: mean: %.4f stddev: %.2f", queryComponent, meanDur, stdDevDur),
		)
	}
	// punting this as we only have tested for one tenant so far
	//return fmt.Sprintf(
	//	"tenant average queue durations:\n%v\n"+"query component average queue durations:\n%v\n",
	//	tenantReports,
	//	queryComponentReports,
	//)
	return fmt.Sprintf(
		"queueDurSecs: %v",
		queryComponentReports,
	)
}

//func percentile(numbers []float64, p float64) float64 {
//	slices.Sort(numbers)
//	index := int(float64(len(numbers)) * p)
//	return numbers[index]
//}

func mean(numbers []float64) float64 {
	sum := 0.0
	for _, number := range numbers {
		sum += number
	}
	return sum / float64(len(numbers))
}

func stddev(numbers []float64, mean float64) float64 {
	sumOfSquares := 0.0
	for _, number := range numbers {
		sumOfSquares += math.Pow(number-mean, 2)
	}
	meanOfSquares := sumOfSquares / float64(len(numbers))
	return math.Sqrt(meanOfSquares)
}

// TestMultiDimensionalQueueFairnessSlowConsumerEffects emulates a simplified queue slowdown scenario
// which the scheduler's additional queue dimensions features are intended to solve for.
//
// In this scenario, one category of queue item causes the queue consumer to slow down, introducing a
// significant delay while the queue consumer processes it and before the consumer can dequeue the next item.
// This emulates a situation where one of the query components - the ingesters or store-gateways - is under load.
//
// If queue items belonging to the slow category are in the same queue in front of the normal queue items,
// the normal queue items must wait for all slow queue items to be cleared before they can be serviced.
// In this way, the degraded performance of the slow query component equally degrades the performance of the
// queries which *could* be serviced quickly, but are waiting behind the slow queries in the queue.
func TestMultiDimensionalQueueAlgorithmSlowConsumerEffects(t *testing.T) {

	weightedQueueDimensionTestCases := []struct {
		name                         string
		tenantQueueDimensionsWeights map[string]map[string]float64
	}{
		{
			name: "1 tenant, 01pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .99,
					storeGatewayQueueDimension:            .005,
					ingesterAndStoreGatewayQueueDimension: .005,
				},
			},
		},
		{
			name: "1 tenant, 05pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .95,
					storeGatewayQueueDimension:            .025,
					ingesterAndStoreGatewayQueueDimension: .025,
				},
			},
		},
		{
			name: "1 tenant, 10pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .90,
					storeGatewayQueueDimension:            .05,
					ingesterAndStoreGatewayQueueDimension: .05,
				},
			},
		},
		{
			name: "1 tenant, 25pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .75,
					storeGatewayQueueDimension:            .125,
					ingesterAndStoreGatewayQueueDimension: .125,
				},
			},
		},
		{
			name: "1 tenant, 50pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .50,
					storeGatewayQueueDimension:            .25,
					ingesterAndStoreGatewayQueueDimension: .25,
				},
			},
		},
		{
			name: "1 tenant, 75pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .25,
					storeGatewayQueueDimension:            .375,
					ingesterAndStoreGatewayQueueDimension: .375,
				},
			},
		},
		{
			name: "1 tenant, 90pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .10,
					storeGatewayQueueDimension:            .45,
					ingesterAndStoreGatewayQueueDimension: .45,
				},
			},
		},
		{
			name: "1 tenant, 95pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .05,
					storeGatewayQueueDimension:            .475,
					ingesterAndStoreGatewayQueueDimension: .475,
				},
			},
		},
		{
			name: "1 tenant, 99pct slow queries",
			tenantQueueDimensionsWeights: map[string]map[string]float64{
				"0": {
					ingesterQueueDimension:                .01,
					storeGatewayQueueDimension:            .495,
					ingesterAndStoreGatewayQueueDimension: .495,
				},
			},
		},
	}
	// capacity reservations less than 0.3 and greater than 0.4 are not tested to minimize test iterations;
	// - previous tests showed that 0.3 improved on all cases less than 0.3,
	//  with no significant negative effects
	// - cases greater than 0.4 showed some minor negative effects
	//  without significantly more positive effects than the 0.4 case
	//reservedQuerierCapacityTestCases := []float64{0.3, 0.4}

	maxQueriersPerTenant := 0 // disable shuffle sharding

	totalRequests := 10000
	numProducers := 10
	numConsumers := 12

	normalConsumerLatency := 1 * time.Millisecond
	slowConsumerQueueDimension := storeGatewayQueueDimension
	// slow request approximately 100x longer than the fast request seems fair;
	// an ingester can respond in 0.3 seconds while a slow store-gateway query can take 30 seconds
	slowConsumerLatency := 100 * time.Millisecond

	var testCaseNames []string
	testCaseReports := map[string]*testScenarioQueueDurationReport{}

	for _, weightedQueueDimensionTestCase := range weightedQueueDimensionTestCases {
		numTenants := len(weightedQueueDimensionTestCase.tenantQueueDimensionsWeights)

		//for reservedCapacityTestIdx, testReservedCapacity := range reservedQuerierCapacityTestCases {
		var err error
		queryComponentUtilization, err := NewQueryComponentUtilization(testQuerierInflightRequestsMetric())
		require.NoError(t, err)

		utilizationCheckThresholdImpl, err := NewQueryComponentUtilizationLimitByConnections(DefaultReservedQueryComponentCapacity)
		require.NoError(t, err)

		queryComponentUtilizationQueueAlgo := queryComponentQueueAlgoSkipOverUtilized{
			utilization:           queryComponentUtilization,
			limit:                 utilizationCheckThresholdImpl,
			currentNodeOrderIndex: 0,
		}
		tqa := newTQA()

		nonFlippedRoundRobinTree, err := NewTree(tqa, &roundRobinState{}, &roundRobinState{})
		require.NoError(t, err)
		queryComponentUtilizationSkipTree, err := NewTree(&queryComponentUtilizationQueueAlgo, tqa, &roundRobinState{})
		require.NoError(t, err)
		querierWorkerPrioritizationTree, err := NewTree(&querierWorkerPrioritizationQueueAlgo{}, tqa, &roundRobinState{})
		require.NoError(t, err)

		trees := []struct {
			name string
			tree Tree
		}{
			{
				"non-flipped round robin tree",
				nonFlippedRoundRobinTree,
			},
			{
				"querier worker priority tree",
				querierWorkerPrioritizationTree,
			},
			{
				"query component skipper tree",
				queryComponentUtilizationSkipTree,
			},
		}
		for _, tree := range trees {
			//if tree.tree != queryComponentUtilizationSkipTree && reservedCapacityTestIdx > 0 {
			//	// other trees don't use this configuration, no need to run the cases
			//	continue
			//}

			//testCaseName := fmt.Sprintf(
			//	"tree: %s, reservedCapacity: %1.2f, %s",
			//	tree.name,
			//	testReservedCapacity,
			//	weightedQueueDimensionTestCase.name,
			//)
			testCaseName := fmt.Sprintf(
				"tree: %s, %s",
				tree.name,
				weightedQueueDimensionTestCase.name,
			)
			testCaseReport := &testScenarioQueueDurationReport{
				componentUtilizationReservedCapacity:    0.0,
				tenantIDQueueDurationObservations:       map[string][]float64{},
				queryComponentQueueDurationObservations: map[string][]float64{},
			}

			// only the non-flipped tree uses the old tenant -> query component hierarchy
			prioritizeQueryComponents := tree.tree != nonFlippedRoundRobinTree

			t.Run(testCaseName, func(t *testing.T) {
				queue, err := newBenchmarkRequestQueue(queryComponentUtilization, tqa, tree.tree, prioritizeQueryComponents)
				require.NoError(t, err)

				ctx := context.Background()
				require.NoError(t, queue.starting(ctx))
				t.Cleanup(func() {
					require.NoError(t, queue.stop(nil))
				})

				// configure queue producers to enqueue requests with the query component
				// assigned according to the weighted queue dimension test case
				queueDimensionFunc := makeWeightedRandAdditionalQueueDimensionFunc(
					weightedQueueDimensionTestCase.tenantQueueDimensionsWeights,
				)
				producersChan, producersErrGroup := makeQueueProducerGroup(
					queue, maxQueriersPerTenant, totalRequests, numProducers, numTenants, queueDimensionFunc,
				)

				// configure queue consumers with sleep for processing queue items
				consumeFunc := makeQueueConsumeFunc(
					queue, slowConsumerQueueDimension, slowConsumerLatency, normalConsumerLatency, testCaseReport,
				)
				queueConsumerErrGroup, startConsumersChan := makeQueueConsumerGroup(
					queue, totalRequests, numConsumers, consumeFunc,
				)

				// run queue consumers and producers and wait for completion

				// start consumers first
				// this allows more time for the dequeue algorithm to operate
				// before the slow requests cause the queue to be backlogged,
				// allowing for more fair comparison between dequeue algorithms
				// which change behavior based on the length of the queue backlog
				close(startConsumersChan)
				close(producersChan)

				// wait for producers and consumers to finish
				err = producersErrGroup.Wait()
				require.NoError(t, err)
				err = queueConsumerErrGroup.Wait()
				require.NoError(t, err)

				t.Logf(testCaseName + ": " + testCaseReport.String())
				testCaseNames = append(testCaseNames, testCaseName)
				testCaseReports[testCaseName] = testCaseReport

				// ensure everything was dequeued
				path, val := tree.tree.Dequeue()
				assert.Nil(t, val)
				assert.Equal(t, path, QueuePath{})
			})
			//}
		}
	}
	for _, testCaseName := range testCaseNames {
		t.Logf(testCaseName + ": " + testCaseReports[testCaseName].String())
	}
}
