package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/metrics"

	"github.com/rs/zerolog/log"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"

	noRecordedValueMask = uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)
)

// otlpMetrics implements OTLP/HTTP metrics export. We decode into
// MetricsData, which the OTLP proto defines as wire-compatible with
// ExportMetricsServiceRequest - the collector package would drag the whole
// gRPC stack into the binary for one HTTP endpoint. Success is 200 + an empty
// ExportMetricsServiceResponse in the request's encoding (zero bytes / `{}`).
func (ar *Router) otlpMetrics(w http.ResponseWriter, req *http.Request) {
	ct := req.Header.Get("Content-Type")
	var isJSON bool
	switch {
	case strings.HasPrefix(ct, contentTypeProtobuf):
	case strings.HasPrefix(ct, contentTypeJSON):
		isJSON = true
	default:
		ar.sendErrorResponse(w, http.StatusUnsupportedMediaType, common.ErrCodeBadRequest)
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxIngestPayloadBytes)
	payload, err := io.ReadAll(req.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			ar.sendErrorResponse(w, http.StatusRequestEntityTooLarge, common.ErrCodePayloadTooLarge)
			return
		}
		ar.sendErrorResponse(w, http.StatusBadRequest, common.ErrCodeBadRequest)
		return
	}

	var exportReq metricspb.MetricsData
	if isJSON {
		err = protojson.Unmarshal(payload, &exportReq)
	} else {
		err = proto.Unmarshal(payload, &exportReq)
	}
	if err != nil {
		ar.sendErrorResponse(w, http.StatusBadRequest, common.ErrCodeBadRequest)
		return
	}

	rows := ar.otlpToRows(&exportReq, time.Now().UnixMilli())
	// a 2 MiB payload of minimal datapoints can explode into hundreds of
	// thousands of rows, and the batch channel holds up to 100 such slices
	if len(rows) > maxOTLPRowsPerRequest {
		ar.sendErrorResponse(w, http.StatusRequestEntityTooLarge, common.ErrCodePayloadTooLarge)
		return
	}
	if !ar.metricsPipeline.TryPush(rows) {
		// pipeline saturated: the OTel exporter retries with backoff
		ar.sendErrorResponse(w, http.StatusTooManyRequests, common.ErrCodeTooManyRequests)
		return
	}

	if isJSON {
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", contentTypeProtobuf)
	w.WriteHeader(http.StatusOK)
}

func (ar *Router) otlpToRows(req *metricspb.MetricsData, receivedAt int64) []metrics.Row {
	var rows []metrics.Row
	for _, rm := range req.GetResourceMetrics() {
		service, host := resourceIdentity(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				rows = ar.appendMetric(rows, m, service, host, receivedAt)
			}
		}
	}
	return rows
}

func (ar *Router) appendMetric(rows []metrics.Row, m *metricspb.Metric, service, host string, receivedAt int64) []metrics.Row {
	if m.GetName() == "" {
		return rows
	}
	base := metrics.Row{Service: service, Host: host}

	switch data := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range data.Gauge.GetDataPoints() {
			rows = appendNumberPoint(rows, base, m.GetName(), metrics.TypeGauge, dp, receivedAt)
		}
	case *metricspb.Metric_Sum:
		typ := metrics.TypeCounter
		if !data.Sum.GetIsMonotonic() {
			typ = metrics.TypeGauge
		}
		for _, dp := range data.Sum.GetDataPoints() {
			rows = appendNumberPoint(rows, base, m.GetName(), typ, dp, receivedAt)
		}
	case *metricspb.Metric_Histogram:
		ar.warnDowncast(m.GetName(), "histogram")
		for _, dp := range data.Histogram.GetDataPoints() {
			rows = appendSumCount(rows, base, m.GetName(), dp.GetSum(), dp.Sum != nil, dp.GetCount(),
				dp.GetAttributes(), dp.GetTimeUnixNano(), dp.GetFlags(), receivedAt)
		}
	case *metricspb.Metric_ExponentialHistogram:
		ar.warnDowncast(m.GetName(), "exponential histogram")
		for _, dp := range data.ExponentialHistogram.GetDataPoints() {
			rows = appendSumCount(rows, base, m.GetName(), dp.GetSum(), dp.Sum != nil, dp.GetCount(),
				dp.GetAttributes(), dp.GetTimeUnixNano(), dp.GetFlags(), receivedAt)
		}
	case *metricspb.Metric_Summary:
		ar.warnDowncast(m.GetName(), "summary")
		for _, dp := range data.Summary.GetDataPoints() {
			rows = appendSumCount(rows, base, m.GetName(), dp.GetSum(), true, dp.GetCount(),
				dp.GetAttributes(), dp.GetTimeUnixNano(), dp.GetFlags(), receivedAt)
		}
	}
	return rows
}

func appendNumberPoint(rows []metrics.Row, base metrics.Row, name string, typ int, dp *metricspb.NumberDataPoint, receivedAt int64) []metrics.Row {
	if dp.GetFlags()&noRecordedValueMask != 0 {
		return rows
	}
	r := base
	r.Name = name
	r.Type = typ
	r.Timestamp = otlpTimestampMs(dp.GetTimeUnixNano(), receivedAt)
	r.Labels = labelsJSON(dp.GetAttributes())
	switch v := dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		r.Value = v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		r.Value = float64(v.AsInt)
	default:
		return rows // point without a value is invalid per spec
	}
	return append(rows, r)
}

// appendSumCount is the lossy histogram/summary downcast: {name}_sum and
// {name}_count, buckets/quantiles dropped. Both are CUMULATIVE (Prometheus
// treats them as counters), so they get the counter type - raw-value charts
// of them draw a straight climb, and the type is what lets the UI say so.
func appendSumCount(rows []metrics.Row, base metrics.Row, name string, sum float64, hasSum bool, count uint64,
	attrs []*commonpb.KeyValue, timeNano uint64, flags uint32, receivedAt int64) []metrics.Row {
	if flags&noRecordedValueMask != 0 {
		return rows
	}
	ts := otlpTimestampMs(timeNano, receivedAt)
	labels := labelsJSON(attrs)
	if hasSum {
		r := base
		r.Name, r.Type, r.Value, r.Timestamp, r.Labels = name+"_sum", metrics.TypeCounter, sum, ts, labels
		rows = append(rows, r)
	}
	r := base
	r.Name, r.Type, r.Value, r.Timestamp, r.Labels = name+"_count", metrics.TypeCounter, float64(count), ts, labels
	return append(rows, r)
}

func (ar *Router) warnDowncast(name, kind string) {
	if _, seen := ar.downcastWarned.LoadOrStore(name, struct{}{}); !seen {
		log.Warn().Str("metric", name).Str("kind", kind).
			Msg("downcasting to _sum/_count counters; export pre-computed quantile gauges if you need percentiles")
	}
}

func resourceIdentity(attrs []*commonpb.KeyValue) (service, host string) {
	service, host = "unknown", "unknown"
	for _, kv := range attrs {
		switch kv.GetKey() {
		case "service.name":
			if s := kv.GetValue().GetStringValue(); s != "" {
				service = s
			}
		case "host.name":
			if h := kv.GetValue().GetStringValue(); h != "" {
				host = h
			}
		}
	}
	return service, host
}

func labelsJSON(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return ""
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() != "" {
			m[kv.GetKey()] = anyValueString(kv.GetValue())
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func anyValueString(v *commonpb.AnyValue) string {
	switch t := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return t.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(t.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(t.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(t.DoubleValue, 'g', -1, 64)
	default:
		// arrays/kvlists/bytes are rare as datapoint attributes; a debug
		// string beats dropping the label entirely
		return fmt.Sprintf("%v", v.GetValue())
	}
}

func otlpTimestampMs(nano uint64, receivedAt int64) int64 {
	if nano == 0 {
		return receivedAt
	}
	return common.ClampTimestamp(int64(nano/1e6), receivedAt)
}
