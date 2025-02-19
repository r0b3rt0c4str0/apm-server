// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package otlp_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/elastic/apm-data/model/modelpb"
	"github.com/elastic/apm-server/internal/beater/interceptors"
	"github.com/elastic/apm-server/internal/beater/otlp"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
)

func TestConsumeTracesGRPC(t *testing.T) {
	var batches []modelpb.Batch
	var reportError error
	var batchProcessor modelpb.ProcessBatchFunc = func(ctx context.Context, batch *modelpb.Batch) error {
		batches = append(batches, *batch)
		return reportError
	}

	conn := newGRPCServer(t, batchProcessor)
	client := ptraceotlp.NewGRPCClient(conn)

	// Send a minimal trace to verify that everything is connected properly.
	//
	// We intentionally do not check the published event contents; those are
	// tested in processor/otel.
	traces := ptrace.NewTraces()
	span := traces.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName("operation_name")

	tracesRequest := ptraceotlp.NewExportRequestFromTraces(traces)
	_, err := client.Export(context.Background(), tracesRequest)
	assert.NoError(t, err)
	require.Len(t, batches, 1)

	reportError = errors.New("failed to publish events")
	_, err = client.Export(context.Background(), tracesRequest)
	assert.Error(t, err)
	errStatus := status.Convert(err)
	assert.Equal(t, "failed to publish events", errStatus.Message())
	require.Len(t, batches, 2)
	assert.Len(t, batches[0], 1)
	assert.Len(t, batches[1], 1)

	actual := map[string]interface{}{}
	monitoring.GetRegistry("apm-server.otlp.grpc.traces").Do(monitoring.Full, func(key string, value interface{}) {
		actual[key] = value
	})
	assert.Equal(t, map[string]interface{}{
		"request.count":                int64(2),
		"response.count":               int64(2),
		"response.errors.count":        int64(1),
		"response.valid.count":         int64(1),
		"response.errors.ratelimit":    int64(0),
		"response.errors.timeout":      int64(0),
		"response.errors.unauthorized": int64(0),
	}, actual)
}

func TestConsumeMetricsGRPC(t *testing.T) {
	var reportError error
	var batchProcessor modelpb.ProcessBatchFunc = func(ctx context.Context, batch *modelpb.Batch) error {
		return reportError
	}

	conn := newGRPCServer(t, batchProcessor)
	client := pmetricotlp.NewGRPCClient(conn)

	// Send a minimal metric to verify that everything is connected properly.
	//
	// We intentionally do not check the published event contents; those are
	// tested in processor/otel.
	metrics := pmetric.NewMetrics()
	metric := metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	metric.SetName("metric_type")
	metric.SetEmptySummary()
	metric.Summary().DataPoints().AppendEmpty()

	metricsRequest := pmetricotlp.NewExportRequestFromMetrics(metrics)
	_, err := client.Export(context.Background(), metricsRequest)
	assert.NoError(t, err)

	reportError = errors.New("failed to publish events")
	_, err = client.Export(context.Background(), metricsRequest)
	assert.Error(t, err)

	errStatus := status.Convert(err)
	assert.Equal(t, "failed to publish events", errStatus.Message())

	actual := map[string]interface{}{}
	monitoring.GetRegistry("apm-server.otlp.grpc.metrics").Do(monitoring.Full, func(key string, value interface{}) {
		actual[key] = value
	})
	assert.Equal(t, map[string]interface{}{
		"consumer.unsupported_dropped": int64(0),

		"request.count":                int64(2),
		"response.count":               int64(2),
		"response.errors.count":        int64(1),
		"response.valid.count":         int64(1),
		"response.errors.ratelimit":    int64(0),
		"response.errors.timeout":      int64(0),
		"response.errors.unauthorized": int64(0),
	}, actual)
}

func TestConsumeLogsGRPC(t *testing.T) {
	var batches []modelpb.Batch
	var reportError error
	var batchProcessor modelpb.ProcessBatchFunc = func(ctx context.Context, batch *modelpb.Batch) error {
		batches = append(batches, *batch)
		return reportError
	}

	conn := newGRPCServer(t, batchProcessor)
	client := plogotlp.NewGRPCClient(conn)

	// Send a minimal log record to verify that everything is connected properly.
	//
	// We intentionally do not check the published event contents; those are
	// tested in processor/otel.
	logs := plog.NewLogs()
	logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()

	logsRequest := plogotlp.NewExportRequestFromLogs(logs)
	_, err := client.Export(context.Background(), logsRequest)
	assert.NoError(t, err)
	require.Len(t, batches, 1)

	reportError = errors.New("failed to publish events")
	_, err = client.Export(context.Background(), logsRequest)
	assert.Error(t, err)
	errStatus := status.Convert(err)
	assert.Equal(t, "failed to publish events", errStatus.Message())
	require.Len(t, batches, 2)
	assert.Len(t, batches[0], 1)
	assert.Len(t, batches[1], 1)

	actual := map[string]interface{}{}
	monitoring.GetRegistry("apm-server.otlp.grpc.logs").Do(monitoring.Full, func(key string, value interface{}) {
		actual[key] = value
	})
	assert.Equal(t, map[string]interface{}{
		"request.count":                int64(2),
		"response.count":               int64(2),
		"response.errors.count":        int64(1),
		"response.valid.count":         int64(1),
		"response.errors.ratelimit":    int64(0),
		"response.errors.timeout":      int64(0),
		"response.errors.unauthorized": int64(0),
	}, actual)
}

func newGRPCServer(t *testing.T, batchProcessor modelpb.BatchProcessor) *grpc.ClientConn {
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	logger := logp.NewLogger("otlp.grpc.test")
	srv := grpc.NewServer(grpc.UnaryInterceptor(interceptors.Metrics(logger, nil)))
	semaphore := semaphore.NewWeighted(1)
	otlp.RegisterGRPCServices(srv, zap.NewNop(), batchProcessor, semaphore)

	go srv.Serve(lis)
	t.Cleanup(srv.GracefulStop)
	conn, err := grpc.Dial(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}
