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

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.elastic.co/apm/v2/apmtest"

	"github.com/elastic/beats/v7/libbeat/common"
	libkibana "github.com/elastic/beats/v7/libbeat/kibana"

	"github.com/elastic/apm-server/agentcfg"
	"github.com/elastic/apm-server/beater/auth"
	"github.com/elastic/apm-server/beater/config"
	"github.com/elastic/apm-server/beater/headers"
	"github.com/elastic/apm-server/beater/request"
	"github.com/elastic/apm-server/kibana"
	"github.com/elastic/apm-server/kibana/kibanatest"
)

type m map[string]interface{}

var (
	mockVersion = *common.MustNewVersion("7.5.0")
	mockEtag    = "1c9588f5a4da71cdef992981a9c9735c"
	successBody = map[string]string{"sampling_rate": "0.5"}
	emptyBody   = map[string]string{}

	testcases = map[string]struct {
		kbClient                               kibana.Client
		requestHeader                          map[string]string
		queryParams                            map[string]string
		method                                 string
		respStatus                             int
		respBody                               map[string]string
		respEtagHeader, respCacheControlHeader string
	}{
		"NotModified": {
			kbClient: kibanatest.MockKibana(http.StatusOK, m{
				"_id": "1",
				"_source": m{
					"settings": m{
						"sampling_rate": 0.5,
					},
					"etag": mockEtag,
				},
			}, mockVersion, true),
			method:                 http.MethodGet,
			requestHeader:          map[string]string{headers.IfNoneMatch: `"` + mockEtag + `"`},
			queryParams:            map[string]string{"service.name": "opbeans-node"},
			respStatus:             http.StatusNotModified,
			respCacheControlHeader: "max-age=4, must-revalidate",
			respEtagHeader:         `"` + mockEtag + `"`,
		},

		"ModifiedWithEtag": {
			kbClient: kibanatest.MockKibana(http.StatusOK, m{
				"_id": "1",
				"_source": m{
					"settings": m{
						"sampling_rate": 0.5,
					},
					"etag": mockEtag,
				},
			}, mockVersion, true),
			method:                 http.MethodGet,
			requestHeader:          map[string]string{headers.IfNoneMatch: "2"},
			queryParams:            map[string]string{"service.name": "opbeans-java"},
			respStatus:             http.StatusOK,
			respEtagHeader:         `"` + mockEtag + `"`,
			respCacheControlHeader: "max-age=4, must-revalidate",
			respBody:               successBody,
		},

		"NoConfigFound": {
			kbClient:               kibanatest.MockKibana(http.StatusNotFound, m{}, mockVersion, true),
			method:                 http.MethodGet,
			queryParams:            map[string]string{"service.name": "opbeans-python"},
			respStatus:             http.StatusOK,
			respCacheControlHeader: "max-age=4, must-revalidate",
			respEtagHeader:         fmt.Sprintf("\"%s\"", agentcfg.EtagSentinel),
			respBody:               emptyBody,
		},

		"SendToKibanaFailed": {
			kbClient:               kibanatest.MockKibana(http.StatusBadGateway, m{}, mockVersion, true),
			method:                 http.MethodGet,
			queryParams:            map[string]string{"service.name": "opbeans-ruby"},
			respStatus:             http.StatusServiceUnavailable,
			respCacheControlHeader: "max-age=300, must-revalidate",
			respBody:               map[string]string{"error": fmt.Sprintf("%s: testerror", agentcfg.ErrMsgSendToKibanaFailed)},
		},

		"NoConnection": {
			kbClient:               kibanatest.MockKibana(http.StatusServiceUnavailable, m{}, mockVersion, false),
			method:                 http.MethodGet,
			queryParams:            map[string]string{"service.name": "opbeans-node"},
			respStatus:             http.StatusServiceUnavailable,
			respCacheControlHeader: "max-age=300, must-revalidate",
			respBody:               map[string]string{"error": agentcfg.ErrMsgNoKibanaConnection},
		},

		"InvalidVersion": {
			kbClient: kibanatest.MockKibana(http.StatusServiceUnavailable, m{},
				*common.MustNewVersion("7.2.0"), true),
			method:                 http.MethodGet,
			queryParams:            map[string]string{"service.name": "opbeans-node"},
			respStatus:             http.StatusServiceUnavailable,
			respCacheControlHeader: "max-age=300, must-revalidate",
			respBody: map[string]string{"error": fmt.Sprintf("%s: min version 7.5.0, "+
				"configured version 7.2.0", agentcfg.ErrMsgKibanaVersionNotCompatible)},
		},

		"NoService": {
			kbClient:               kibanatest.MockKibana(http.StatusOK, m{}, mockVersion, true),
			method:                 http.MethodGet,
			respStatus:             http.StatusBadRequest,
			respBody:               map[string]string{"error": "service.name is required"},
			respCacheControlHeader: "max-age=300, must-revalidate",
		},

		"MethodNotAllowed": {
			kbClient:               kibanatest.MockKibana(http.StatusOK, m{}, mockVersion, true),
			method:                 http.MethodPut,
			respStatus:             http.StatusMethodNotAllowed,
			respCacheControlHeader: "max-age=300, must-revalidate",
			respBody:               map[string]string{"error": fmt.Sprintf("%s: PUT", msgMethodUnsupported)},
		},

		"Unauthorized": {
			kbClient:               kibanatest.MockKibana(http.StatusUnauthorized, m{"error": "Unauthorized"}, mockVersion, true),
			method:                 http.MethodGet,
			queryParams:            map[string]string{"service.name": "opbeans-node"},
			respStatus:             http.StatusServiceUnavailable,
			respCacheControlHeader: "max-age=300, must-revalidate",
			respBody: map[string]string{"error": "APM Server is not authorized to query Kibana. " +
				"Please configure apm-server.kibana.username and apm-server.kibana.password, " +
				"and ensure the user has the necessary privileges."},
		},
	}
)

func TestAgentConfigHandler(t *testing.T) {
	var cfg = config.KibanaAgentConfig{Cache: config.Cache{Expiration: 4 * time.Second}}
	for _, tc := range testcases {
		f := agentcfg.NewKibanaFetcher(tc.kbClient, cfg.Cache.Expiration)
		h := NewHandler(f, cfg, "", nil)
		r := httptest.NewRequest(tc.method, target(tc.queryParams), nil)
		for k, v := range tc.requestHeader {
			r.Header.Set(k, v)
		}
		ctx, w := newRequestContext(r)
		h(ctx)

		require.Equal(t, tc.respStatus, w.Code)
		require.Equal(t, tc.respCacheControlHeader, w.Header().Get(headers.CacheControl))
		require.Equal(t, tc.respEtagHeader, w.Header().Get(headers.Etag))
		b, err := ioutil.ReadAll(w.Body)
		require.NoError(t, err)
		var actualBody map[string]string
		json.Unmarshal(b, &actualBody)
		assert.Equal(t, tc.respBody, actualBody)
	}
}

func TestAgentConfigHandlerAnonymousAccess(t *testing.T) {
	kbClient := kibanatest.MockKibana(http.StatusUnauthorized, m{"error": "Unauthorized"}, mockVersion, true)
	cfg := config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(kbClient, cfg.Cache.Expiration)
	h := NewHandler(f, cfg, "", nil)

	for _, tc := range []struct {
		anonymous    bool
		response     string
		authResource *auth.Resource
	}{{
		anonymous:    false,
		response:     `{"error":"APM Server is not authorized to query Kibana. Please configure apm-server.kibana.username and apm-server.kibana.password, and ensure the user has the necessary privileges."}`,
		authResource: &auth.Resource{ServiceName: "opbeans"},
	}, {
		anonymous:    true,
		response:     `{"error":"Unauthorized"}`,
		authResource: &auth.Resource{ServiceName: "opbeans"},
	}} {
		r := httptest.NewRequest(http.MethodGet, target(map[string]string{"service.name": "opbeans"}), nil)
		c, w := newRequestContext(r)

		c.Authentication.Method = "none"
		if tc.anonymous {
			c.Authentication.Method = ""
		}

		var requestedResource *auth.Resource
		c.Request = withAuthorizer(c.Request,
			authorizerFunc(func(ctx context.Context, action auth.Action, resource auth.Resource) error {
				if requestedResource != nil {
					panic("expected only one Authorize request")
				}
				requestedResource = &resource
				return nil
			}),
		)
		h(c)
		assert.Equal(t, tc.response+"\n", w.Body.String())
		assert.Equal(t, tc.authResource, requestedResource)
	}
}

func TestAgentConfigHandlerAuthorizedForService(t *testing.T) {
	cfg := config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(nil, cfg.Cache.Expiration)
	h := NewHandler(f, cfg, "", nil)

	r := httptest.NewRequest(http.MethodGet, target(map[string]string{"service.name": "opbeans"}), nil)
	ctx, w := newRequestContext(r)

	var queriedResource auth.Resource
	ctx.Request = withAuthorizer(ctx.Request,
		authorizerFunc(func(ctx context.Context, action auth.Action, resource auth.Resource) error {
			queriedResource = resource
			return auth.ErrUnauthorized
		}),
	)
	h(ctx)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, auth.Resource{ServiceName: "opbeans"}, queriedResource)
}

func TestAgentConfigHandler_NoKibanaClient(t *testing.T) {
	cfg := config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(nil, cfg.Cache.Expiration)
	h := NewHandler(f, cfg, "", nil)

	w := sendRequest(h, httptest.NewRequest(http.MethodPost, "/config", jsonReader(m{
		"service": m{"name": "opbeans-node"}})))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestAgentConfigHandler_PostOk(t *testing.T) {
	kb := kibanatest.MockKibana(http.StatusOK, m{
		"_id": "1",
		"_source": m{
			"settings": m{
				"sampling_rate": 0.5,
			},
		},
	}, mockVersion, true)

	var cfg = config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(kb, cfg.Cache.Expiration)
	h := NewHandler(f, cfg, "", nil)

	w := sendRequest(h, httptest.NewRequest(http.MethodPost, "/config", jsonReader(m{
		"service": m{"name": "opbeans-node"}})))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestAgentConfigHandler_DefaultServiceEnvironment(t *testing.T) {
	kb := &recordingKibanaClient{
		Client: kibanatest.MockKibana(http.StatusOK, m{
			"_id": "1",
			"_source": m{
				"settings": m{
					"sampling_rate": 0.5,
				},
			},
		}, mockVersion, true),
	}

	var cfg = config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(kb, cfg.Cache.Expiration)
	h := NewHandler(f, cfg, "default", nil)

	sendRequest(h, httptest.NewRequest(http.MethodPost, "/config", jsonReader(m{"service": m{"name": "opbeans-node", "environment": "specified"}})))
	sendRequest(h, httptest.NewRequest(http.MethodPost, "/config", jsonReader(m{"service": m{"name": "opbeans-node"}})))
	require.Len(t, kb.requests, 2)

	body0, _ := ioutil.ReadAll(kb.requests[0].Body)
	body1, _ := ioutil.ReadAll(kb.requests[1].Body)
	assert.Equal(t, `{"service":{"name":"opbeans-node","environment":"specified"},"etag":""}`+"\n", string(body0))
	assert.Equal(t, `{"service":{"name":"opbeans-node","environment":"default"},"etag":""}`+"\n", string(body1))
}

func TestAgentConfigRum(t *testing.T) {
	h := getHandler("rum-js")
	r := httptest.NewRequest(http.MethodPost, "/rum", jsonReader(m{
		"service": m{"name": "opbeans"}}))
	ctx, w := newRequestContext(r)
	ctx.Authentication.Method = "" // unauthenticated
	h(ctx)
	var actual map[string]string
	json.Unmarshal(w.Body.Bytes(), &actual)
	assert.Equal(t, headers.Etag, w.Header().Get(headers.AccessControlExposeHeaders))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, map[string]string{"transaction_sample_rate": "0.5"}, actual)
}

func TestAgentConfigRumEtag(t *testing.T) {
	h := getHandler("rum-js")
	r := httptest.NewRequest(http.MethodGet, "/rum?ifnonematch=123&service.name=opbeans", nil)
	ctx, w := newRequestContext(r)
	h(ctx)
	assert.Equal(t, http.StatusNotModified, w.Code, w.Body.String())
}

func TestAgentConfigNotRum(t *testing.T) {
	h := getHandler("node-js")
	r := httptest.NewRequest(http.MethodPost, "/backend", jsonReader(m{
		"service": m{"name": "opbeans"}}))
	ctx, w := newRequestContext(r)
	ctx.Request = withAuthorizer(ctx.Request,
		authorizerFunc(func(context.Context, auth.Action, auth.Resource) error {
			return nil
		}),
	)
	h(ctx)
	var actual map[string]string
	json.Unmarshal(w.Body.Bytes(), &actual)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, map[string]string{"capture_body": "transactions", "transaction_sample_rate": "0.5"}, actual)
}

func TestAgentConfigNoLeak(t *testing.T) {
	h := getHandler("node-js")
	r := httptest.NewRequest(http.MethodPost, "/rum", jsonReader(m{
		"service": m{"name": "opbeans"}}))
	ctx, w := newRequestContext(r)
	ctx.Authentication.Method = "" // unauthenticated
	h(ctx)
	var actual map[string]string
	json.Unmarshal(w.Body.Bytes(), &actual)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, map[string]string{}, actual)
}

func getHandler(agent string) request.Handler {
	kb := kibanatest.MockKibana(http.StatusOK, m{
		"_id": "1",
		"_source": m{
			"settings": m{
				"transaction_sample_rate": 0.5,
				"capture_body":            "transactions",
			},
			"etag":       "123",
			"agent_name": agent,
		},
	}, mockVersion, true)
	cfg := config.KibanaAgentConfig{Cache: config.Cache{Expiration: time.Nanosecond}}
	f := agentcfg.NewKibanaFetcher(kb, cfg.Cache.Expiration)
	return NewHandler(f, cfg, "", []string{"rum-js"})
}

func TestIfNoneMatch(t *testing.T) {
	var fromHeader = func(s string) *request.Context {
		r := &http.Request{Header: map[string][]string{"If-None-Match": {s}}}
		return &request.Context{Request: r}
	}

	var fromQueryArg = func(s string) *request.Context {
		r := &http.Request{}
		r.URL, _ = url.Parse("http://host:8200/path?ifnonematch=123")
		return &request.Context{Request: r}
	}

	assert.Equal(t, "123", ifNoneMatch(fromHeader("123")))
	assert.Equal(t, "123", ifNoneMatch(fromHeader(`"123"`)))
	assert.Equal(t, "123", ifNoneMatch(fromQueryArg("123")))
}

func TestAgentConfigTraceContext(t *testing.T) {
	kibanaCfg := config.KibanaConfig{Enabled: true, ClientConfig: libkibana.DefaultClientConfig()}
	kibanaCfg.Host = "testKibana:12345"
	client := kibana.NewConnectingClient(&kibanaCfg)
	cfg := config.KibanaAgentConfig{Cache: config.Cache{Expiration: 5 * time.Minute}}
	f := agentcfg.NewKibanaFetcher(client, cfg.Cache.Expiration)
	handler := NewHandler(f, cfg, "default", nil)
	_, spans, _ := apmtest.WithTransaction(func(ctx context.Context) {
		// When the handler is called with a context containing
		// a transaction, the underlying Kibana query should create a span
		r := httptest.NewRequest(http.MethodPost, "/backend", jsonReader(m{
			"service": m{"name": "opbeans"}}))
		sendRequest(handler, r.WithContext(ctx))
	})
	require.Len(t, spans, 1)
	assert.Equal(t, "app", spans[0].Type)
}

func sendRequest(h request.Handler, r *http.Request) *httptest.ResponseRecorder {
	ctx, recorder := newRequestContext(r)
	ctx.Request = withAuthorizer(ctx.Request,
		authorizerFunc(func(context.Context, auth.Action, auth.Resource) error {
			return nil
		}),
	)
	h(ctx)
	return recorder
}

func newRequestContext(r *http.Request) (*request.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	ctx := request.NewContext()
	ctx.Reset(w, r)
	ctx.Request = withAnonymousAuthorizer(ctx.Request)
	ctx.Authentication.Method = auth.MethodNone
	return ctx, w
}

func target(params map[string]string) string {
	t := "/config"
	if len(params) == 0 {
		return t
	}
	t += "?"
	for k, v := range params {
		t = fmt.Sprintf("%s%s=%s", t, k, v)
	}
	return t
}

type recordingKibanaClient struct {
	kibana.Client
	requests []*http.Request
}

func (c *recordingKibanaClient) Send(ctx context.Context, method string, path string, params url.Values, header http.Header, body io.Reader) (*http.Response, error) {
	req := httptest.NewRequest(method, path, body)
	req.URL.RawQuery = params.Encode()
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	c.requests = append(c.requests, req.WithContext(ctx))
	return c.Client.Send(ctx, method, path, params, header, body)
}

func withAnonymousAuthorizer(req *http.Request) *http.Request {
	return withAuthorizer(req, authorizerFunc(func(context.Context, auth.Action, auth.Resource) error {
		return nil
	}))
}

func withAuthorizer(req *http.Request, authz auth.Authorizer) *http.Request {
	return req.WithContext(auth.ContextWithAuthorizer(req.Context(), authz))
}

type authorizerFunc func(context.Context, auth.Action, auth.Resource) error

func (f authorizerFunc) Authorize(ctx context.Context, action auth.Action, resource auth.Resource) error {
	return f(ctx, action, resource)
}

func jsonReader(v interface{}) io.Reader {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(data)
}
