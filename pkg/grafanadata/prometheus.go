package grafanadata

import "strings"

// ConvertResultToPrometheusFormat converts a Grafana data response into prometheus format
func ConvertResultToPrometheusFormat(results Results) PrometheusMetricResponse {
	promResponse := PrometheusMetricResponse{
		Status: "success",
		Data: PrometheusMetricData{
			ResultType: "matrix",
		},
	}

	for ref, result := range results.Results {
		for _, frame := range result.Frames {
			var promResult PrometheusMetricDataResult

			metricLabels := map[string]string{
				"__refId__": ref,
			}

			legend := results.Legends[ref]

			for _, field := range frame.Schema.Fields {
				for labelKey, labelValue := range field.Labels {
					metricLabels[labelKey] = labelValue
					if legend != "" {
						legend = strings.ReplaceAll(legend, "{{"+labelKey+"}}", labelValue)
					}
				}
			}

			metricLabels["__legend__"] = legend

			promResult.Metric = metricLabels
			if len(frame.Data.Values) >= 2 {
				timestamps := frame.Data.Values[0]
				values := frame.Data.Values[1]

				for index, timestamp := range timestamps {
					if index < len(values) {
						value := values[index]
						promResult.Values = append(promResult.Values, []interface{}{timestamp / 1000, value})
					}
				}
			}

			promResponse.Data.Result = append(promResponse.Data.Result, promResult)
		}
	}

	return promResponse
}
