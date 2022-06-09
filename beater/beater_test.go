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

package beater

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/elastic/apm-server/beater/config"
	"github.com/elastic/apm-server/elasticsearch"
	"github.com/elastic/apm-server/model"
	"github.com/elastic/apm-server/model/modelindexer/modelindexertest"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/idxmgmt"
	"github.com/elastic/beats/v7/libbeat/instrumentation"
	"github.com/elastic/beats/v7/libbeat/outputs"
	agentconfig "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
)

type testBeater struct {
	*beater
	b     *beat.Beat
	logs  *observer.ObservedLogs
	runCh chan error

	listenAddr string
	baseURL    string
	client     *http.Client
}

func setupServer(t *testing.T, cfg *agentconfig.C, beatConfig *beat.BeatConfig, docsOut chan<- []byte) (*testBeater, error) {
	if testing.Short() {
		t.Skip("skipping server test")
	}
	apmBeat, cfg := newBeat(t, cfg, beatConfig, docsOut)
	return setupBeater(t, apmBeat, cfg, beatConfig)
}

func newBeat(t *testing.T, cfg *agentconfig.C, beatConfig *beat.BeatConfig, docsOut chan<- []byte) (*beat.Beat, *agentconfig.C) {
	info := beat.Info{
		Beat:        "test-apm-server",
		IndexPrefix: "test-apm-server",
		Version:     "1.2.3", // hard-coded to avoid changing approvals
		ID:          uuid.Must(uuid.FromString("fbba762a-14dd-412c-b7e9-b79f903eb492")),
	}

	combinedConfig := agentconfig.MustNewConfigFrom(map[string]interface{}{
		"host": "localhost:0",

		// Disable waiting for integration to be installed by default,
		// to simplify tests. This feature is tested independently.
		"data_streams.wait_for_integration": false,
	})
	if cfg != nil {
		require.NoError(t, cfg.Unpack(combinedConfig))
	}

	var pub beat.Pipeline
	if docsOut != nil {
		// Clear the state monitoring registry to avoid panicking due
		// to double registration.
		stateRegistry := monitoring.GetNamespace("state").GetRegistry()
		stateRegistry.Clear()
		defer stateRegistry.Clear()

		// beatConfig must be nil if an event channel is supplied.
		require.Nil(t, beatConfig)
		beatConfig = &beat.BeatConfig{}

		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			// We must send a valid JSON response for the initial
			// Elasticsearch cluster UUID query.
			fmt.Fprintln(w, `{"version":{"number":"1.2.3"}}`)
		})
		modelindexertest.HandleBulk(mux, func(w http.ResponseWriter, r *http.Request) {
			docs, response := modelindexertest.DecodeBulkRequest(r)
			defer json.NewEncoder(w).Encode(response)
			for _, doc := range docs {
				var buf bytes.Buffer
				if err := json.Indent(&buf, doc, "", "  "); err != nil {
					panic(err)
				}
				select {
				case <-r.Context().Done():
				case docsOut <- buf.Bytes():
				}
			}
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		err := beatConfig.Output.Unpack(agentconfig.MustNewConfigFrom(map[string]interface{}{
			"elasticsearch": map[string]interface{}{
				"enabled":      true,
				"hosts":        []string{srv.URL},
				"flush_bytes":  "1", // no delay
				"max_requests": "1", // only 1 concurrent request, for event ordering
			},
		}))
		require.NoError(t, err)
	}

	if beatConfig != nil && beatConfig.Output.Name() == "elasticsearch" {
		// capture events using the configured elasticsearch output
		supporter, err := idxmgmt.DefaultSupport(logp.NewLogger("beater_test"), info, nil)
		require.NoError(t, err)
		outputGroup, err := outputs.Load(supporter, info, nil, "elasticsearch", beatConfig.Output.Config())
		require.NoError(t, err)
		pub = dummyPipeline(cfg, info, outputGroup.Clients...)
	} else {
		// don't capture events
		pub = dummyPipeline(cfg, info)
	}

	instrumentation, err := instrumentation.New(combinedConfig, info.Beat, info.Version)
	require.NoError(t, err)
	return &beat.Beat{
		Publisher:       pub,
		Info:            info,
		Config:          beatConfig,
		Instrumentation: instrumentation,
	}, combinedConfig
}

func setupBeater(
	t *testing.T,
	apmBeat *beat.Beat,
	ucfg *agentconfig.C,
	beatConfig *beat.BeatConfig,
) (*testBeater, error) {
	tb, err := newTestBeater(t, apmBeat, ucfg, beatConfig)
	if err != nil {
		return nil, err
	}
	tb.start()

	listenAddr, err := tb.waitListenAddr(10 * time.Second)
	if err != nil {
		return nil, err
	}
	tb.initClient(tb.config, listenAddr)

	res, err := tb.client.Get(tb.baseURL)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	return tb, nil
}

func newTestBeater(
	t *testing.T,
	apmBeat *beat.Beat,
	ucfg *agentconfig.C,
	beatConfig *beat.BeatConfig,
) (*testBeater, error) {

	core, observedLogs := observer.New(zapcore.DebugLevel)
	logger := logp.NewLogger("", zap.WrapCore(func(in zapcore.Core) zapcore.Core {
		return zapcore.NewTee(in, core)
	}))

	createBeater := NewCreator(CreatorParams{
		Logger: logger,
		WrapRunServer: func(runServer RunServerFunc) RunServerFunc {
			var processor model.ProcessBatchFunc = func(ctx context.Context, batch *model.Batch) error {
				for i := range *batch {
					event := &(*batch)[i]
					if event.Processor != model.TransactionProcessor {
						continue
					}
					// Add a label to test that everything
					// goes through the wrapped reporter.
					if event.Labels == nil {
						event.Labels = make(model.Labels)
					}
					event.Labels.Set("wrapped_reporter", "true")
				}
				return nil
			}
			return WrapRunServerWithProcessors(runServer, processor)
		},
	})

	beatBeater, err := createBeater(apmBeat, ucfg)
	if err != nil {
		return nil, err
	}
	require.NotNil(t, beatBeater)
	t.Cleanup(func() {
		beatBeater.Stop()
	})

	return &testBeater{
		beater: beatBeater.(*beater),
		b:      apmBeat,
		logs:   observedLogs,
		runCh:  make(chan error),
	}, nil
}

// start starts running a beater created with newTestBeater.
func (tb *testBeater) start() {
	go func() {
		tb.runCh <- tb.beater.Run(tb.b)
	}()
}

func (tb *testBeater) waitListenAddr(timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	for {
		for _, entry := range tb.logs.TakeAll() {
			const prefix = "Listening on: "
			if strings.HasPrefix(entry.Message, prefix) {
				listenAddr := entry.Message[len(prefix):]
				return listenAddr, nil
			}
		}
		select {
		case err := <-tb.runCh:
			if err != nil {
				return "", err
			}
			return "", errors.New("server exited cleanly without logging expected message")
		case <-deadline:
			return "", errors.New("timeout waiting for server to start listening")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (tb *testBeater) initClient(cfg *config.Config, listenAddr string) {
	tb.listenAddr = listenAddr
	transport := &http.Transport{}
	if parsed, err := url.Parse(cfg.Host); err == nil && parsed.Scheme == "unix" {
		transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", parsed.Path)
		}
		tb.baseURL = "http://test-apm-server/"
		tb.client = &http.Client{Transport: transport}
	} else {
		scheme := "http://"
		if cfg.TLS.IsEnabled() {
			scheme = "https://"
		}
		tb.baseURL = scheme + listenAddr
		tb.client = &http.Client{Transport: transport}
	}
}

func TestSourcemapIndexPattern(t *testing.T) {
	test := func(t *testing.T, indexPattern, expected string) {
		var requestPaths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestPaths = append(requestPaths, r.URL.Path)
		}))
		defer srv.Close()

		cfg := config.DefaultConfig()
		cfg.RumConfig.Enabled = true
		cfg.RumConfig.SourceMapping.ESConfig.Hosts = []string{srv.URL}
		if indexPattern != "" {
			cfg.RumConfig.SourceMapping.IndexPattern = indexPattern
		}

		fetcher, err := newSourcemapFetcher(
			beat.Info{Version: "1.2.3"}, cfg.RumConfig.SourceMapping, nil,
			nil, elasticsearch.NewClient,
		)
		require.NoError(t, err)
		fetcher.Fetch(context.Background(), "name", "version", "path")
		require.Len(t, requestPaths, 1)

		path := requestPaths[0]
		path = strings.TrimPrefix(path, "/")
		path = strings.TrimSuffix(path, "/_search")
		assert.Equal(t, expected, path)
	}
	t.Run("default-pattern", func(t *testing.T) { test(t, "", "apm-*-sourcemap*") })
	t.Run("with-observer-version", func(t *testing.T) { test(t, "blah-%{[observer.version]}-blah", "blah-1.2.3-blah") })
}

var validSourcemap, _ = ioutil.ReadFile("../testdata/sourcemap/bundle.js.map")

func TestStoreUsesRUMElasticsearchConfig(t *testing.T) {
	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Write(validSourcemap)
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.RumConfig.Enabled = true
	cfg.RumConfig.SourceMapping.Enabled = true
	cfg.RumConfig.SourceMapping.ESConfig = elasticsearch.DefaultConfig()
	cfg.RumConfig.SourceMapping.ESConfig.Hosts = []string{ts.URL}

	fetcher, err := newSourcemapFetcher(
		beat.Info{Version: "1.2.3"}, cfg.RumConfig.SourceMapping, nil,
		nil, elasticsearch.NewClient,
	)
	require.NoError(t, err)
	// Check that the provided rum elasticsearch config was used and
	// Fetch() goes to the test server.
	_, err = fetcher.Fetch(context.Background(), "app", "1.0", "/bundle/path")
	require.NoError(t, err)

	assert.True(t, called)
}

func TestFleetStoreUsed(t *testing.T) {
	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		wr := zlib.NewWriter(w)
		defer wr.Close()
		wr.Write([]byte(fmt.Sprintf(`{"sourceMap":%s}`, validSourcemap)))
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.RumConfig.Enabled = true
	cfg.RumConfig.SourceMapping.Enabled = true
	cfg.RumConfig.SourceMapping.Metadata = []config.SourceMapMetadata{{
		ServiceName:    "app",
		ServiceVersion: "1.0",
		BundleFilepath: "/bundle/path",
		SourceMapURL:   "/my/path",
	}}

	fleetCfg := &config.Fleet{
		Hosts:        []string{ts.URL[7:]},
		Protocol:     "http",
		AccessAPIKey: "my-key",
		TLS:          nil,
	}

	fetcher, err := newSourcemapFetcher(beat.Info{Version: "1.2.3"}, cfg.RumConfig.SourceMapping, fleetCfg, nil, nil)
	require.NoError(t, err)
	_, err = fetcher.Fetch(context.Background(), "app", "1.0", "/bundle/path")
	require.NoError(t, err)

	assert.True(t, called)
}

func TestQueryClusterUUIDRegistriesExist(t *testing.T) {
	stateRegistry := monitoring.GetNamespace("state").GetRegistry()
	stateRegistry.Clear()
	defer stateRegistry.Clear()

	elasticsearchRegistry := stateRegistry.NewRegistry("outputs.elasticsearch")
	monitoring.NewString(elasticsearchRegistry, "cluster_uuid")

	ctx := context.Background()
	clusterUUID := "abc123"
	client := &mockClusterUUIDClient{ClusterUUID: clusterUUID}
	err := queryClusterUUID(ctx, client)
	require.NoError(t, err)

	fs := monitoring.CollectFlatSnapshot(elasticsearchRegistry, monitoring.Full, false)
	assert.Equal(t, clusterUUID, fs.Strings["cluster_uuid"])
}

func TestQueryClusterUUIDRegistriesDoNotExist(t *testing.T) {
	stateRegistry := monitoring.GetNamespace("state").GetRegistry()
	stateRegistry.Clear()
	defer stateRegistry.Clear()

	ctx := context.Background()
	clusterUUID := "abc123"
	client := &mockClusterUUIDClient{ClusterUUID: clusterUUID}
	err := queryClusterUUID(ctx, client)
	require.NoError(t, err)

	elasticsearchRegistry := stateRegistry.GetRegistry("outputs.elasticsearch")
	require.NotNil(t, elasticsearchRegistry)

	fs := monitoring.CollectFlatSnapshot(elasticsearchRegistry, monitoring.Full, false)
	assert.Equal(t, clusterUUID, fs.Strings["cluster_uuid"])
}

func TestAdjustMaxProcsTickerRefresh(t *testing.T) {
	// This test asserts that the GOMAXPROCS is called multiple times
	// respecting the time.Duration that is passed in the function.
	for _, maxP := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("%d_GOMAXPROCS", maxP), func(t *testing.T) {
			observedLogs := testAdjustMaxProcs(t, maxP, false)
			assert.GreaterOrEqual(t, observedLogs.Len(), 10)
		})
	}
}

func TestAdjustMaxProcsTickerRefreshDiffLogger(t *testing.T) {
	// This test asserts that the log messages aren't logged more than once.
	for _, maxP := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("%d_GOMAXPROCS", maxP), func(t *testing.T) {
			observedLogs := testAdjustMaxProcs(t, maxP, true)
			// Assert that only 1 message has been logged.
			assert.Equal(t, observedLogs.Len(), 1)
		})
	}
}

func testAdjustMaxProcs(t *testing.T, maxP int, diffCore bool) *observer.ObservedLogs {
	t.Setenv("GOMAXPROCS", fmt.Sprint(maxP))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	core, observedLogs := observer.New(zapcore.DebugLevel)
	logger := logp.NewLogger("", zap.WrapCore(func(in zapcore.Core) zapcore.Core {
		return zapcore.NewTee(in, core)
	}))

	// Adjust maxprocs every 1ms.
	refreshDuration := time.Millisecond
	logFunc := logger.Infof
	if diffCore {
		logFunc = diffInfof(logger)
	}

	go adjustMaxProcs(ctx, refreshDuration, logFunc, logger.Errorf)

	filterMsg := fmt.Sprintf(`maxprocs: Honoring GOMAXPROCS="%d"`, maxP)
	for {
		select {
		// Wait for 50ms so adjustmaxprocs has had time to run a few times.
		case <-time.After(50 * refreshDuration):
			logs := observedLogs.FilterMessageSnippet(filterMsg)
			if logs.Len() >= 1 {
				return logs
			}
		case <-ctx.Done():
			t.Error(ctx.Err())
			return nil
		}
	}
}

type mockClusterUUIDClient struct {
	ClusterUUID string `json:"cluster_uuid"`
}

func (c *mockClusterUUIDClient) Perform(r *http.Request) (*http.Response, error) {
	m, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	resp := &http.Response{
		StatusCode: 200,
		Body:       &mockReadCloser{bytes.NewReader(m)},
		Request:    r,
	}
	return resp, nil
}

func (c *mockClusterUUIDClient) NewBulkIndexer(_ elasticsearch.BulkIndexerConfig) (elasticsearch.BulkIndexer, error) {
	return nil, nil
}

type mockReadCloser struct{ r io.Reader }

func (r *mockReadCloser) Read(p []byte) (int, error) { return r.r.Read(p) }
func (r *mockReadCloser) Close() error               { return nil }
