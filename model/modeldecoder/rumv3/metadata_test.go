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

package rumv3

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/apm-server/decoder"
	"github.com/elastic/apm-server/model"
	"github.com/elastic/apm-server/model/modeldecoder/modeldecodertest"
)

// initializedMetadata returns a model.APMEvent populated with default values
// in the metadata-derived fields.
func initializedMetadata() model.APMEvent {
	var input metadata
	var out model.APMEvent
	modeldecodertest.SetStructValues(&input, modeldecodertest.DefaultValues(), func(key string, field, value reflect.Value) bool {
		return key != "Experimental"
	})
	mapToMetadataModel(&input, &out)
	// initialize values that are not set by input
	out.UserAgent = model.UserAgent{Name: "init", Original: "init"}
	out.Client.Domain = "init"
	out.Client.IP = net.ParseIP("127.0.0.1")
	out.Client.Port = 1
	nat := &model.NAT{IP: net.ParseIP("127.0.0.1")}
	out.Source = model.Source{IP: out.Client.IP, Port: out.Client.Port, Domain: out.Client.Domain, NAT: nat}
	return out
}

func metadataExceptions(keys ...string) func(key string) bool {
	missing := []string{
		"Agent",
		"Child",
		"Cloud",
		"Container",
		"DataStream",
		"Destination",
		"ECSVersion",
		"FAAS",
		"FAAS.ID",
		"FAAS.Coldstart",
		"FAAS.Execution",
		"FAAS.TriggerType",
		"FAAS.TriggerRequestID",
		"FAAS.Name",
		"FAAS.Version",
		"Experimental",
		"HTTP",
		"Kubernetes",
		"Message",
		"Network",
		"Observer",
		"Origin",
		"Target",
		"Parent",
		"Process",
		"Processor",
		"Service.Node",
		"Service.Agent.EphemeralID",
		"Host",
		"Event",
		"Service.Origin",
		"Service.Target",
		"Session",
		"Trace",
		"URL",
		"Log",

		// Dedicated test for it.
		"NumericLabels",
		"Labels",

		// event-specific fields
		"Error",
		"Metricset",
		"ProfileSample",
		"Span",
		"Transaction",
	}
	exceptions := append(missing, keys...)
	return func(key string) bool {
		for _, k := range exceptions {
			if strings.HasPrefix(key, k) {
				return true
			}
		}
		return false
	}
}

func TestMetadataResetModelOnRelease(t *testing.T) {
	inp := `{"m":{"se":{"n":"service-a"}}}`
	m := fetchMetadataRoot()
	require.NoError(t, decoder.NewJSONDecoder(strings.NewReader(inp)).Decode(m))
	require.True(t, m.IsSet())
	releaseMetadataRoot(m)
	assert.False(t, m.IsSet())
}

func TestDecodeNestedMetadata(t *testing.T) {
	t.Run("decode", func(t *testing.T) {
		var out model.APMEvent
		testMinValidMetadata := `{"m":{"se":{"n":"name","a":{"n":"go","ve":"1.0.0"}}}}`
		dec := decoder.NewJSONDecoder(strings.NewReader(testMinValidMetadata))
		require.NoError(t, DecodeNestedMetadata(dec, &out))
		assert.Equal(t, model.APMEvent{
			Service: model.Service{Name: "name"},
			Agent:   model.Agent{Name: "go", Version: "1.0.0"},
		}, out)

		err := DecodeNestedMetadata(decoder.NewJSONDecoder(strings.NewReader(`malformed`)), &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode")
	})

	t.Run("validate", func(t *testing.T) {
		inp := `{}`
		var out model.APMEvent
		err := DecodeNestedMetadata(decoder.NewJSONDecoder(strings.NewReader(inp)), &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "validation")
	})

	t.Run("labels", func(t *testing.T) {
		var out model.APMEvent
		labelMetadata := `{"m":{"l":{"a":"b","c":true,"d":1234,"e":1234.11},"se":{"n":"name","a":{"n":"go","ve":"1.0.0"}}}}`
		dec := decoder.NewJSONDecoder(strings.NewReader(labelMetadata))
		require.NoError(t, DecodeNestedMetadata(dec, &out))
		assert.Equal(t, model.APMEvent{
			Service: model.Service{Name: "name"},
			Agent:   model.Agent{Name: "go", Version: "1.0.0"},
			Labels: model.Labels{
				"a": {Value: "b"},
				"c": {Value: "true"},
			},
			NumericLabels: model.NumericLabels{
				"d": {Value: float64(1234)},
				"e": {Value: float64(1234.11)},
			},
		}, out)
	})
}

func TestDecodeMetadataMappingToModel(t *testing.T) {
	expected := func(s string, ip net.IP, n int) model.APMEvent {
		labels := model.Labels{}
		for i := 0; i < n; i++ {
			labels[fmt.Sprintf("%s%v", s, i)] = model.LabelValue{Value: s}
		}
		return model.APMEvent{
			Agent: model.Agent{Name: s, Version: s},
			Service: model.Service{Name: s, Version: s, Environment: s,
				Language:  model.Language{Name: s, Version: s},
				Runtime:   model.Runtime{Name: s, Version: s},
				Framework: model.Framework{Name: s, Version: s}},
			User:   model.User{Name: s, Email: s, Domain: s, ID: s},
			Labels: labels,
			Network: model.Network{
				Connection: model.NetworkConnection{
					Type: s,
				},
			},
			// these values are not set from http headers and
			// are not expected change with updated input data
			UserAgent: model.UserAgent{Original: "init", Name: "init"},
			Client: model.Client{
				Domain: "init",
				IP:     net.ParseIP("127.0.0.1"),
				Port:   1,
			},
			Source: model.Source{
				Domain: "init",
				IP:     net.ParseIP("127.0.0.1"),
				Port:   1,
				NAT:    &model.NAT{IP: net.ParseIP("127.0.0.1")},
			},
		}
	}

	t.Run("overwrite", func(t *testing.T) {
		// setup:
		// create initialized modeldecoder and empty model metadata
		// map modeldecoder to model metadata and manually set
		// enhanced data that are never set by the modeldecoder
		out := initializedMetadata()
		// iterate through model and assert values are set
		defaultVal := modeldecodertest.DefaultValues()
		assert.Equal(t, expected(defaultVal.Str, defaultVal.IP, defaultVal.N), out)

		// overwrite model metadata with specified Values
		// then iterate through model and assert values are overwritten
		var input metadata
		otherVal := modeldecodertest.NonDefaultValues()
		modeldecodertest.SetStructValues(&input, otherVal)
		mapToMetadataModel(&input, &out)
		assert.Equal(t, expected(otherVal.Str, otherVal.IP, otherVal.N), out)

		// map an empty modeldecoder metadata to the model
		// and assert values are unchanged
		input.Reset()
		modeldecodertest.SetZeroStructValues(&input)
		mapToMetadataModel(&input, &out)
		assert.Equal(t, expected(otherVal.Str, otherVal.IP, otherVal.N), out)
	})

	t.Run("reused-memory", func(t *testing.T) {
		var input metadata
		var out1, out2 model.APMEvent
		defaultVal := modeldecodertest.DefaultValues()
		modeldecodertest.SetStructValues(&input, defaultVal)
		mapToMetadataModel(&input, &out1)
		// initialize values that are not set by input
		out1.UserAgent = model.UserAgent{Name: "init", Original: "init"}
		out1.Client.Domain = "init"
		out1.Client.IP = net.ParseIP("127.0.0.1")
		out1.Client.Port = 1
		nat := &model.NAT{IP: out1.Client.IP}
		out1.Source = model.Source{IP: out1.Client.IP, Port: out1.Client.Port, Domain: out1.Client.Domain, NAT: nat}
		assert.Equal(t, expected(defaultVal.Str, defaultVal.IP, defaultVal.N), out1)

		// overwrite model metadata with specified Values
		// then iterate through model and assert values are overwritten
		otherVal := modeldecodertest.NonDefaultValues()
		input.Reset()
		modeldecodertest.SetStructValues(&input, otherVal)
		mapToMetadataModel(&input, &out2)
		out2.UserAgent = model.UserAgent{Name: "init", Original: "init"}
		out2.Client.Domain = "init"
		out2.Client.IP = net.ParseIP("127.0.0.1")
		out2.Client.Port = 1
		out2.Source = model.Source{IP: out2.Client.IP, Port: out2.Client.Port, Domain: out2.Client.Domain, NAT: nat}
		assert.Equal(t, expected(otherVal.Str, otherVal.IP, otherVal.N), out2)
		assert.Equal(t, expected(defaultVal.Str, defaultVal.IP, defaultVal.N), out1)
	})
}
