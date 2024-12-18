package erpc

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var tmu sync.Mutex

func TestHttpServer_RaceTimeouts(t *testing.T) {
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})
	defer gock.Off()

	// Setup
	logger := zerolog.New(zerolog.NewConsoleWriter())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: "500ms", // Set a very short timeout for testing
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 1,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 1,
						},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
	require.NoError(t, err)

	httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

	// Start the server on a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()
	defer httpServer.server.Shutdown()

	// Wait for the server to start
	time.Sleep(1000 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func() (int, string) {
		body := strings.NewReader(
			fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["%d", false],"id":1}`, rand.Intn(100000000)),
		)
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{
			Timeout: 1000 * time.Millisecond, // Set a client timeout longer than the server timeout
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, err.Error()
		}
		return resp.StatusCode, string(respBody)
	}

	t.Run("ConcurrentRequestsWithTimeouts", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(50).
			Reply(200).
			Delay(2000 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		const concurrentRequests = 5
		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, concurrentRequests)

		for i := 0; i < concurrentRequests; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
		}

		wg.Wait()

		for _, result := range results {
			if result.statusCode != http.StatusGatewayTimeout {
				t.Errorf("unexpected status code: %d", result.statusCode)
			}
			assert.Contains(t, result.body, "timeout")
		}
	})

	t.Run("RapidSuccessiveRequests", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(10).
			Reply(200).
			Delay(700 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		for i := 0; i < 10; i++ {
			statusCode, body := sendRequest()
			assert.Equal(t, http.StatusGatewayTimeout, statusCode)
			assert.Contains(t, body, "timeout")
		}
	})

	t.Run("MixedTimeoutAndNonTimeoutRequests", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		totalReqs := 10

		for i := 0; i < totalReqs; i++ {
			var delay time.Duration
			if i%2 == 0 {
				delay = 1 * time.Millisecond // shorter than the server timeout
			} else {
				delay = 700 * time.Millisecond // longer than the server timeout
			}
			gock.New("http://rpc1.localhost").
				Post("/").
				Times(1).
				ReplyFunc(func(r *gock.Response) {
					r.Status(200)
					r.JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      rand.Intn(100_000_000),
						"result": map[string]interface{}{
							"blockNumber": rand.Intn(100000000),
						},
					})
				}).
				Delay(delay)
		}

		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, totalReqs)

		for i := 0; i < totalReqs; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
			time.Sleep(50 * time.Millisecond)
		}
		wg.Wait()

		timeouts := 0
		successes := 0
		for _, result := range results {
			if result.statusCode == http.StatusGatewayTimeout {
				timeouts++
				assert.Contains(t, result.body, "timeout")
			} else {
				successes++
				assert.Contains(t, result.body, "blockNumber")
			}
		}

		fmt.Printf("Timeouts: %d, Successes: %d\n", timeouts, successes)
		assert.True(t, timeouts > 0, "Expected some timeouts")
		assert.True(t, successes > 0, "Expected some successes")
	})
}

func TestHttpServer_SingleUpstream(t *testing.T) {
	type testCase struct {
		name      string
		configure func(cfg *common.Config)
	}

	// Define your test cases
	testCases := []testCase{
		{
			name: "UpstreamSupportsBatch",
			configure: func(cfg *common.Config) {
				cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.TRUE
			},
		},
		{
			name: "UpstreamDoesNotSupportBatch",
			configure: func(cfg *common.Config) {
				cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.FALSE
			},
		},
		{
			name: "CachingEnabled",
			configure: func(cfg *common.Config) {
				cfg.Database = &common.DatabaseConfig{
					EvmJsonRpcCache: &common.ConnectorConfig{
						Driver: "memory",
						Memory: &common.MemoryConnectorConfig{
							MaxItems: 100,
						},
					},
				}
			},
		},
		{
			name: "CachingDisabled",
			configure: func(cfg *common.Config) {
				cfg.Database = &common.DatabaseConfig{
					EvmJsonRpcCache: nil,
				}
			},
		},
	}

	for _, tc := range testCases {
		// Capture the current value of tc
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := &common.Config{
				Server: &common.ServerConfig{
					MaxTimeout: "5s",
				},
				Projects: []*common.ProjectConfig{
					{
						Id: "test_project",
						Networks: []*common.NetworkConfig{
							{
								Architecture: common.ArchitectureEvm,
								Evm: &common.EvmNetworkConfig{
									ChainId: 1,
								},
							},
						},
						Upstreams: []*common.UpstreamConfig{
							{
								Type:     common.UpstreamTypeEvm,
								Endpoint: "http://rpc1.localhost",
								Evm: &common.EvmUpstreamConfig{
									ChainId: 1,
								},
								VendorName: "llama",
								JsonRpc: &common.JsonRpcUpstreamConfig{
									SupportsBatch: &common.FALSE,
								},
							},
						},
					},
				},
				RateLimiters: &common.RateLimiterConfig{},
			}

			if tc.configure != nil {
				tc.configure(cfg)
			}

			// Set up test fixtures
			sendRequest, _, baseURL := createServerTestFixtures(cfg, t)

			t.Run("ConcurrentEthGetBlockNumber", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				const concurrentRequests = 10

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(concurrentRequests).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x444444",
					})

				var wg sync.WaitGroup
				results := make([]struct {
					statusCode int
					body       string
				}, concurrentRequests)

				for i := 0; i < concurrentRequests; i++ {
					wg.Add(1)
					go func(index int) {
						defer wg.Done()
						body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[%d],"id":1}`, index)
						results[index].statusCode, results[index].body = sendRequest(body, nil, nil)
					}(i)
				}

				wg.Wait()

				for i, result := range results {
					assert.Equal(t, http.StatusOK, result.statusCode, "Status code should be 200 for request %d", i)

					var response map[string]interface{}
					err := sonic.Unmarshal([]byte(result.body), &response)
					assert.NoError(t, err, "Should be able to decode response for request %d", i)
					assert.Equal(t, "0x444444", response["result"], "Unexpected result for request %d", i)
				}

				assert.True(t, gock.IsDone(), "All mocks should have been called")
			})

			t.Run("InvalidJSON", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				statusCode, body := sendRequest(`{"invalid json`, nil, nil)

				// fmt.Println(body)

				assert.Equal(t, http.StatusBadRequest, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				errStr, _ := sonic.Marshal(errorObj)
				assert.Contains(t, string(errStr), "failed to parse")
			})

			t.Run("UnsupportedMethod", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{}

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							sg = "[" + sg + "]"
						}
						res.Body = io.NopCloser(strings.NewReader(sg))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"unsupported_method","params":[],"id":1}`, nil, nil)
				assert.Equal(t, http.StatusUnsupportedMediaType, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Equal(t, float64(-32601), errorObj["code"])
			})

			t.Run("IgnoredMethod", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{"ignored_method"}

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							sg = "[" + sg + "]"
						}
						res.Body = io.NopCloser(strings.NewReader(sg))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"ignored_method","params":[],"id":1}`, nil, nil)
				assert.Equal(t, http.StatusUnsupportedMediaType, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Equal(t, float64(-32601), errorObj["code"])
			})

			// Test case: Request with invalid project ID
			t.Run("InvalidProjectID", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				req, err := http.NewRequest("POST", baseURL+"/invalid_project/evm/1", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				client := &http.Client{
					Timeout: 10 * time.Second,
				}
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				assert.Equal(t, http.StatusNotFound, resp.StatusCode)

				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)

				var errorResponse map[string]interface{}
				err = sonic.Unmarshal(body, &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Contains(t, errorObj["message"], "project not configured")
			})

			t.Run("UpstreamLatencyAndTimeout", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Delay(6 * time.Second). // Delay longer than the server timeout
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1111111",
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusGatewayTimeout, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				errStr, _ := sonic.Marshal(errorObj)
				assert.Contains(t, string(errStr), "timeout")

				assert.True(t, gock.IsDone(), "All mocks should have been called")
			})

			t.Run("UnexpectedPlainErrorResponseFromUpstream", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					Reply(200).
					BodyString("error code: 1015")

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusTooManyRequests, statusCode)
				assert.Contains(t, body, "error code: 1015")

				assert.True(t, gock.IsDone(), "All mocks should have been called")
			})

			t.Run("UnexpectedServerErrorResponseFromUpstream", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					Reply(500).
					BodyString(`{"error":{"code":-39999,"message":"my funky error"}}`)

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusInternalServerError, statusCode)
				assert.Contains(t, body, "-32603")
				assert.Contains(t, body, "my funky error")

				assert.True(t, gock.IsDone(), "All mocks should have been called")
			})

			t.Run("MissingIDInJsonRpcRequest", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				var id interface{}
				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						idNode, _ := sonic.Get(bodyBytes, "id")
						id, _ = idNode.Interface()
						if id == nil {
							idNode, _ = sonic.Get(bodyBytes, 0, "id")
							id, _ = idNode.Interface()
						}
						return true, nil
					}).
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						var respTxt string
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							respTxt = `[{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}]`
						} else {
							respTxt = `{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}`
						}
						idp, err := sonic.Marshal(id)
						require.NoError(t, err)
						res.Body = io.NopCloser(strings.NewReader(strings.Replace(respTxt, "THIS_WILL_BE_REPLACED", string(idp), 1)))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[]}`, nil, nil)

				assert.Equal(t, http.StatusOK, statusCode)
				assert.Contains(t, body, "0x123456")

				assert.True(t, gock.IsDone(), "All mocks should have been called")
			})

			t.Run("AutoAddIDandJSONRPCFieldstoRequest", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						bodyStr := string(bodyBytes)
						if !strings.Contains(bodyStr, "\"id\"") {
							t.Fatalf("No id found in request")
						}
						if !strings.Contains(bodyStr, "\"jsonrpc\"") {
							t.Fatalf("No jsonrpc found in request")
						}
						if !strings.Contains(bodyStr, "\"method\"") {
							t.Fatalf("No method found in request")
						}
						if !strings.Contains(bodyStr, "\"params\"") {
							t.Fatalf("No params found in request")
						}
						return true, nil
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x123456"}`)

				sendRequest(`{"method":"eth_traceDebug","params":[]}`, nil, nil)
			})

			t.Run("AlwaysPropagateUpstreamErrorDataField", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(400).
					BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":{"range":"the range 55074203 - 55124202 exceeds the range allowed for your plan (49999 > 2000)."}}}`)

				_, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1}`, nil, nil)
				assert.Contains(t, body, "the range 55074203 - 55124202")
			})

			t.Run("AutoAddIDWhen0IsProvided", func(t *testing.T) {
				tmu.Lock()
				defer tmu.Unlock()
				defer gock.Off()
				defer gock.Clean()

				gock.New("http://rpc1.localhost").
					Post("/").
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						bodyStr := string(bodyBytes)
						if !strings.Contains(bodyStr, "\"id\"") {
							t.Fatalf("No id found in request")
						}
						if bodyStr[0] == '[' {
							idNode, err := sonic.Get(bodyBytes, 0, "id")
							require.NoError(t, err)
							id, err := idNode.Int64()
							require.NoError(t, err)
							if id == 0 {
								t.Fatalf("Expected id to be 0, got %d from body: %s", id, bodyStr)
							}
						} else {
							idNode, err := sonic.Get(bodyBytes, "id")
							require.NoError(t, err)
							id, err := idNode.Int64()
							require.NoError(t, err)
							if id == 0 {
								t.Fatalf("Expected id to be 0, got %d from body: %s", id, bodyStr)
							}
						}
						return true, nil
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x123456"}`)

				sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[],"id":0}`, nil, nil)
			})
		})
	}
}

func TestHttpServer_MultipleUpstreams(t *testing.T) {
	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: "5s",
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 1,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 1,
						},
						VendorName: "llama",
					},
					{
						Id:       "rpc2",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc2.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 1,
						},
						VendorName: "",
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	sendRequest, _, _ := createServerTestFixtures(cfg, t)

	t.Run("UpstreamNotAllowedByDirectiveViaHeaders", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      222,
				"result":  "0x2222222",
			})

		statusCode2, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, map[string]string{
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")

		statusCode1, body1 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode1)
		assert.Contains(t, body1, "0x1111111")

		assert.True(t, gock.IsDone(), "All mocks should have been called")
	})

	t.Run("UpstreamNotAllowedByDirectiveViaQueryParams", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      222,
				"result":  "0x2222222",
			})

		statusCode2, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, map[string]string{
			"use-upstream": "rpc2",
		})

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")

		statusCode1, body1 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode1)
		assert.Contains(t, body1, "0x1111111")

		assert.True(t, gock.IsDone(), "All mocks should have been called")
	})
}

func TestHttpServer_IntegrationTests(t *testing.T) {
	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: "5s",
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				CORS: &common.CORSConfig{
					AllowedOrigins: []string{"https://erpc.cloud"},
					AllowedMethods: []string{"POST"},
				},
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 1,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "drpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 1,
						},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	sendRequest, sendOptionsRequest, _ := createServerTestFixtures(cfg, t)

	t.Run("DrpcUnsupportedMethod", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		gock.New("https://lb.drpc.org").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "the method trace_transaction does not exist/is not available",
				},
			})
		statusCode, _ := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","params":[],"id":111}`, nil, nil)
		assert.Equal(t, http.StatusBadRequest, statusCode)
	})

	t.Run("ReturnCorrectCORS", func(t *testing.T) {
		tmu.Lock()
		defer tmu.Unlock()
		defer gock.Off()
		defer gock.Clean()

		statusCode, headers, _ := sendOptionsRequest("https://erpc.cloud")
		assert.Equal(t, http.StatusNoContent, statusCode)
		assert.Equal(t, "https://erpc.cloud", headers["Access-Control-Allow-Origin"])
		assert.Equal(t, "POST", headers["Access-Control-Allow-Methods"])
	})
}

func createServerTestFixtures(cfg *common.Config, t *testing.T) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, string),
	func(host string) (int, map[string]string, string),
	string,
) {
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	logger := zerolog.New(zerolog.NewConsoleWriter())
	ctx := context.Background()

	erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
	require.NoError(t, err)

	httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()

	time.Sleep(1000 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func(body string, headers map[string]string, queryParams map[string]string) (int, string) {
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		q := req.URL.Query()
		for k, v := range queryParams {
			q.Add(k, v)
		}
		req.URL.RawQuery = q.Encode()

		client := &http.Client{
			Timeout: 10 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, err.Error()
		}
		return resp.StatusCode, string(respBody)
	}

	sendOptionsRequest := func(host string) (int, map[string]string, string) {
		req, err := http.NewRequest("OPTIONS", baseURL+"/test_project/evm/1", nil)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Header.Set("Host", host)
			req.Header.Set("Origin", host)
		}
		client := &http.Client{
			Timeout: 10 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, nil, err.Error()
		}

		headers := make(map[string]string)
		for k, v := range resp.Header {
			headers[k] = v[0]
		}
		return resp.StatusCode, headers, string(respBody)
	}

	return sendRequest, sendOptionsRequest, baseURL
}
