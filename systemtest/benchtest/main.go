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

package benchtest

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.elastic.co/apm/v2/stacktrace"
)

const waitInactiveTimeout = 30 * time.Second

// events holds the current stored events.
//go:embed events/*.ndjson
var events embed.FS

// BenchmarkFunc is the benchmark function type accepted by Run.
type BenchmarkFunc func(*testing.B)

const benchmarkFuncPrefix = "Benchmark"

type benchmark struct {
	name string
	f    BenchmarkFunc
}

func runBenchmark(f func(b *testing.B)) (testing.BenchmarkResult, bool, error) {
	// Run the benchmark. testing.Benchmark will invoke the function
	// multiple times, but only returns the final result.
	var ok bool
	var before, after expvar
	result := testing.Benchmark(func(b *testing.B) {
		if err := queryExpvar(&before); err != nil {
			b.Error(err)
			ok = !b.Failed()
			return
		}
		b.ResetTimer()
		f(b)
		for !b.Failed() {
			if err := queryExpvar(&after); err != nil {
				b.Error(err)
				break
			}
			if after.ActiveEvents == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		ok = !b.Failed()
	})
	if result.Extra != nil {
		addExpvarMetrics(result, before, after)
	}
	return result, ok, nil
}

func addExpvarMetrics(result testing.BenchmarkResult, before, after expvar) {
	result.MemAllocs = after.MemStats.Mallocs - before.MemStats.Mallocs
	result.MemBytes = after.MemStats.TotalAlloc - before.MemStats.TotalAlloc
	result.Bytes = after.UncompressedBytes - before.UncompressedBytes
	result.Extra["events/sec"] = float64(after.TotalEvents-before.TotalEvents) / result.T.Seconds()
	result.Extra["txs/sec"] = float64(after.TransactionsProcessed-before.TransactionsProcessed) / result.T.Seconds()
	result.Extra["spans/sec"] = float64(after.SpansProcessed-before.SpansProcessed) / result.T.Seconds()
	result.Extra["metrics/sec"] = float64(after.MetricsProcessed-before.MetricsProcessed) / result.T.Seconds()
	result.Extra["errors/sec"] = float64(after.ErrorsProcessed-before.ErrorsProcessed) / result.T.Seconds()

	// Record the number of error responses returned by the server: lower is better.
	errorResponsesAfter := after.ErrorElasticResponses + after.ErrorOTLPTracesResponses + after.ErrorOTLPMetricsResponses
	errorResponsesBefore := before.ErrorElasticResponses + before.ErrorOTLPTracesResponses + before.ErrorOTLPMetricsResponses
	errorResponses := errorResponsesAfter - errorResponsesBefore
	result.Extra["error_responses/sec"] = float64(errorResponses) / result.T.Seconds()
}

func fullBenchmarkName(name string, agents int) string {
	if agents != 1 {
		return fmt.Sprintf("%s-%d", name, agents)
	}
	return name
}

func benchmarkFuncName(f BenchmarkFunc) (string, error) {
	ffunc := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if ffunc == nil {
		return "", errors.New("runtime.FuncForPC returned nil")
	}
	fullName := ffunc.Name()
	_, name := stacktrace.SplitFunctionName(fullName)
	if !strings.HasPrefix(name, benchmarkFuncPrefix) {
		return "", fmt.Errorf("benchmark function names must begin with %q (got %q)", fullName, benchmarkFuncPrefix)
	}
	return name, nil
}

// Run runs the given benchmarks according to the flags defined.
//
// Run expects to receive statically-defined functions whose names
// are all prefixed with "Benchmark", like those that are designed
// to work with "go test".
func Run(allBenchmarks ...BenchmarkFunc) error {
	if err := parseFlags(); err != nil {
		return err
	}
	// Sets the http.DefaultClient.Transport.TLSClientConfig.InsecureSkipVerify
	// to match the "-secure" flag value.
	verifyTLS := *secure
	http.DefaultClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS},
	}
	os.Setenv("ELASTIC_APM_VERIFY_SERVER_CERT", fmt.Sprint(verifyTLS))
	var profiles profiles
	if err := profiles.init(); err != nil {
		return err
	}
	defer func() {
		if err := profiles.writeProfiles(); err != nil {
			log.Printf("failed to write profiles: %s", err)
		}
	}()

	var matchRE *regexp.Regexp
	if *match != "" {
		re, err := regexp.Compile(*match)
		if err != nil {
			return err
		}
		matchRE = re
	}
	benchmarks := make([]benchmark, 0, len(allBenchmarks))
	for _, benchmarkFunc := range allBenchmarks {
		name, err := benchmarkFuncName(benchmarkFunc)
		if err != nil {
			return err
		}
		if matchRE == nil || matchRE.MatchString(name) {
			benchmarks = append(benchmarks, benchmark{
				name: name,
				f:    benchmarkFunc,
			})
		}
	}
	sort.Slice(benchmarks, func(i, j int) bool {
		return benchmarks[i].name < benchmarks[j].name
	})

	var maxLen int
	for _, agents := range agentsList {
		for _, benchmark := range benchmarks {
			if n := len(fullBenchmarkName(benchmark.name, agents)); n > maxLen {
				maxLen = n
			}
		}
	}

	// Warmup the APM Server before beggining the benchmarks.
	if h, err := newEventHandler(`.*.ndjson`); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.WarmUpServer(ctx, *warmupEvents); err != nil {
			return fmt.Errorf("failed warming up apm-server: %w", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), waitInactiveTimeout)
		defer cancel()
		if err := WaitUntilServerInactive(ctx); err != nil {
			return fmt.Errorf("received error waiting for server inactive: %w", err)
		}
	}

	for _, agents := range agentsList {
		runtime.GOMAXPROCS(int(agents))
		for _, benchmark := range benchmarks {
			name := fullBenchmarkName(benchmark.name, agents)
			for i := 0; i < int(*count); i++ {
				profileChan := profiles.record(name)
				result, ok, err := runBenchmark(benchmark.f)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintf(os.Stderr, "--- FAIL: %s\n", name)
					return fmt.Errorf("benchmark %q failed", name)
				} else {
					fmt.Fprintf(os.Stderr, "%-*s\t%s\t%s\n", maxLen, name, result, result.MemString())
				}
				if err := <-profileChan; err != nil {
					return err
				}
			}
		}
	}
	return nil
}
