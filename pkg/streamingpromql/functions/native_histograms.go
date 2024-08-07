// SPDX-License-Identifier: AGPL-3.0-only

package functions

import (
	"github.com/prometheus/prometheus/promql"

	"github.com/grafana/mimir/pkg/streamingpromql/limiting"
	"github.com/grafana/mimir/pkg/streamingpromql/types"
)

func HistogramCount(seriesData types.InstantVectorSeriesData, memoryConsumptionTracker *limiting.MemoryConsumptionTracker) (types.InstantVectorSeriesData, error) {
	floats, err := types.FPointSlicePool.Get(len(seriesData.Histograms), memoryConsumptionTracker)
	if err != nil {
		return types.InstantVectorSeriesData{}, err
	}

	data := types.InstantVectorSeriesData{
		Floats: floats,
	}

	for _, histogram := range seriesData.Histograms {
		data.Floats = append(data.Floats, promql.FPoint{
			T: histogram.T,
			F: histogram.H.Count,
		})
	}

	types.PutInstantVectorSeriesData(seriesData, memoryConsumptionTracker)

	return data, nil
}

func HistogramSum(seriesData types.InstantVectorSeriesData, memoryConsumptionTracker *limiting.MemoryConsumptionTracker) (types.InstantVectorSeriesData, error) {
	floats, err := types.FPointSlicePool.Get(len(seriesData.Histograms), memoryConsumptionTracker)
	if err != nil {
		return types.InstantVectorSeriesData{}, err
	}

	data := types.InstantVectorSeriesData{
		Floats: floats,
	}

	for _, histogram := range seriesData.Histograms {
		data.Floats = append(data.Floats, promql.FPoint{
			T: histogram.T,
			F: histogram.H.Sum,
		})
	}

	types.PutInstantVectorSeriesData(seriesData, memoryConsumptionTracker)

	return data, nil
}
