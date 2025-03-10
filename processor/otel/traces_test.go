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

// Portions copied from OpenTelemetry Collector (contrib), from the
// elastic exporter.
//
// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otel_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jaegermodel "github.com/jaegertracing/jaeger/model"
	jaegertranslator "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/model/pdata"
	semconv "go.opentelemetry.io/collector/model/semconv/v1.5.0"
	"google.golang.org/grpc/codes"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/logp"

	"github.com/elastic/apm-server/approvaltest"
	"github.com/elastic/apm-server/beater/beatertest"
	"github.com/elastic/apm-server/model"
	"github.com/elastic/apm-server/processor/otel"
)

func TestConsumer_ConsumeTraces_Empty(t *testing.T) {
	var processor model.ProcessBatchFunc = func(ctx context.Context, batch *model.Batch) error {
		assert.Empty(t, batch)
		return nil
	}

	consumer := otel.Consumer{Processor: processor}
	traces := pdata.NewTraces()
	assert.NoError(t, consumer.ConsumeTraces(context.Background(), traces))
}

func TestOutcome(t *testing.T) {
	test := func(t *testing.T, expectedOutcome, expectedResult string, statusCode pdata.StatusCode) {
		t.Helper()

		traces, spans := newTracesSpans()
		otelSpan1 := spans.Spans().AppendEmpty()
		otelSpan1.SetTraceID(pdata.NewTraceID([16]byte{1}))
		otelSpan1.SetSpanID(pdata.NewSpanID([8]byte{2}))
		otelSpan1.Status().SetCode(statusCode)
		otelSpan2 := spans.Spans().AppendEmpty()
		otelSpan2.SetTraceID(pdata.NewTraceID([16]byte{1}))
		otelSpan2.SetSpanID(pdata.NewSpanID([8]byte{2}))
		otelSpan2.SetParentSpanID(pdata.NewSpanID([8]byte{3}))
		otelSpan2.Status().SetCode(statusCode)

		batch := transformTraces(t, traces)
		require.Len(t, batch, 2)

		assert.Equal(t, expectedOutcome, batch[0].Event.Outcome)
		assert.Equal(t, expectedResult, batch[0].Transaction.Result)
		assert.Equal(t, expectedOutcome, batch[1].Event.Outcome)
	}

	test(t, "unknown", "", pdata.StatusCodeUnset)
	test(t, "success", "Success", pdata.StatusCodeOk)
	test(t, "failure", "Error", pdata.StatusCodeError)
}

func TestRepresentativeCount(t *testing.T) {
	traces, spans := newTracesSpans()
	otelSpan1 := spans.Spans().AppendEmpty()
	otelSpan1.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan1.SetSpanID(pdata.NewSpanID([8]byte{2}))
	otelSpan2 := spans.Spans().AppendEmpty()
	otelSpan2.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan2.SetSpanID(pdata.NewSpanID([8]byte{2}))
	otelSpan2.SetParentSpanID(pdata.NewSpanID([8]byte{3}))

	batch := transformTraces(t, traces)
	require.Len(t, batch, 2)

	assert.Equal(t, 1.0, batch[0].Transaction.RepresentativeCount)
	assert.Equal(t, 1.0, batch[1].Span.RepresentativeCount)
}

func TestHTTPTransactionURL(t *testing.T) {
	test := func(t *testing.T, expected model.URL, attrs map[string]pdata.AttributeValue) {
		t.Helper()
		event := transformTransactionWithAttributes(t, attrs)
		assert.Equal(t, expected, event.URL)
	}

	t.Run("scheme_host_target", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "/foo?bar",
			Full:     "https://testing.invalid:80/foo?bar",
			Path:     "/foo",
			Query:    "bar",
			Domain:   "testing.invalid",
			Port:     80,
		}, map[string]pdata.AttributeValue{
			"http.scheme": pdata.NewAttributeValueString("https"),
			"http.host":   pdata.NewAttributeValueString("testing.invalid:80"),
			"http.target": pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("scheme_servername_nethostport_target", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "/foo?bar",
			Full:     "https://testing.invalid:80/foo?bar",
			Path:     "/foo",
			Query:    "bar",
			Domain:   "testing.invalid",
			Port:     80,
		}, map[string]pdata.AttributeValue{
			"http.scheme":      pdata.NewAttributeValueString("https"),
			"http.server_name": pdata.NewAttributeValueString("testing.invalid"),
			"net.host.port":    pdata.NewAttributeValueInt(80),
			"http.target":      pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("scheme_nethostname_nethostport_target", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "/foo?bar",
			Full:     "https://testing.invalid:80/foo?bar",
			Path:     "/foo",
			Query:    "bar",
			Domain:   "testing.invalid",
			Port:     80,
		}, map[string]pdata.AttributeValue{
			"http.scheme":   pdata.NewAttributeValueString("https"),
			"net.host.name": pdata.NewAttributeValueString("testing.invalid"),
			"net.host.port": pdata.NewAttributeValueInt(80),
			"http.target":   pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("http.url", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "https://testing.invalid:80/foo?bar",
			Full:     "https://testing.invalid:80/foo?bar",
			Path:     "/foo",
			Query:    "bar",
			Domain:   "testing.invalid",
			Port:     80,
		}, map[string]pdata.AttributeValue{
			"http.url": pdata.NewAttributeValueString("https://testing.invalid:80/foo?bar"),
		})
	})
	t.Run("host_no_port", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "/foo",
			Full:     "https://testing.invalid/foo",
			Path:     "/foo",
			Domain:   "testing.invalid",
		}, map[string]pdata.AttributeValue{
			"http.scheme": pdata.NewAttributeValueString("https"),
			"http.host":   pdata.NewAttributeValueString("testing.invalid"),
			"http.target": pdata.NewAttributeValueString("/foo"),
		})
	})
	t.Run("ipv6_host_no_port", func(t *testing.T) {
		test(t, model.URL{
			Scheme:   "https",
			Original: "/foo",
			Full:     "https://[::1]/foo",
			Path:     "/foo",
			Domain:   "::1",
		}, map[string]pdata.AttributeValue{
			"http.scheme": pdata.NewAttributeValueString("https"),
			"http.host":   pdata.NewAttributeValueString("[::1]"),
			"http.target": pdata.NewAttributeValueString("/foo"),
		})
	})
	t.Run("default_scheme", func(t *testing.T) {
		// scheme is set to "http" if it can't be deduced from attributes.
		test(t, model.URL{
			Scheme:   "http",
			Original: "/foo",
			Full:     "http://testing.invalid/foo",
			Path:     "/foo",
			Domain:   "testing.invalid",
		}, map[string]pdata.AttributeValue{
			"http.host":   pdata.NewAttributeValueString("testing.invalid"),
			"http.target": pdata.NewAttributeValueString("/foo"),
		})
	})
}

func TestHTTPSpanURL(t *testing.T) {
	test := func(t *testing.T, expected string, attrs map[string]pdata.AttributeValue) {
		t.Helper()
		event := transformSpanWithAttributes(t, attrs)
		assert.Equal(t, model.URL{Original: expected}, event.URL)
	}

	t.Run("host.url", func(t *testing.T) {
		test(t, "https://testing.invalid:80/foo?bar", map[string]pdata.AttributeValue{
			"http.url": pdata.NewAttributeValueString("https://testing.invalid:80/foo?bar"),
		})
	})
	t.Run("scheme_host_target", func(t *testing.T) {
		test(t, "https://testing.invalid:80/foo?bar", map[string]pdata.AttributeValue{
			"http.scheme": pdata.NewAttributeValueString("https"),
			"http.host":   pdata.NewAttributeValueString("testing.invalid:80"),
			"http.target": pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("scheme_netpeername_netpeerport_target", func(t *testing.T) {
		test(t, "https://testing.invalid:80/foo?bar", map[string]pdata.AttributeValue{
			"http.scheme":   pdata.NewAttributeValueString("https"),
			"net.peer.name": pdata.NewAttributeValueString("testing.invalid"),
			"net.peer.ip":   pdata.NewAttributeValueString("::1"), // net.peer.name preferred
			"net.peer.port": pdata.NewAttributeValueInt(80),
			"http.target":   pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("scheme_netpeerip_netpeerport_target", func(t *testing.T) {
		test(t, "https://[::1]:80/foo?bar", map[string]pdata.AttributeValue{
			"http.scheme":   pdata.NewAttributeValueString("https"),
			"net.peer.ip":   pdata.NewAttributeValueString("::1"),
			"net.peer.port": pdata.NewAttributeValueInt(80),
			"http.target":   pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("default_scheme", func(t *testing.T) {
		// scheme is set to "http" if it can't be deduced from attributes.
		test(t, "http://testing.invalid/foo", map[string]pdata.AttributeValue{
			"http.host":   pdata.NewAttributeValueString("testing.invalid"),
			"http.target": pdata.NewAttributeValueString("/foo"),
		})
	})
}

func TestHTTPSpanDestination(t *testing.T) {
	test := func(t *testing.T, expectedDestination model.Destination, expectedDestinationService *model.DestinationService, attrs map[string]pdata.AttributeValue) {
		t.Helper()
		event := transformSpanWithAttributes(t, attrs)
		assert.Equal(t, expectedDestination, event.Destination)
		assert.Equal(t, expectedDestinationService, event.Span.DestinationService)
	}

	t.Run("url_default_port_specified", func(t *testing.T) {
		test(t, model.Destination{
			Address: "testing.invalid",
			Port:    443,
		}, &model.DestinationService{
			Type:     "external",
			Name:     "https://testing.invalid",
			Resource: "testing.invalid:443",
		}, map[string]pdata.AttributeValue{
			"http.url": pdata.NewAttributeValueString("https://testing.invalid:443/foo?bar"),
		})
	})
	t.Run("url_port_scheme", func(t *testing.T) {
		test(t, model.Destination{
			Address: "testing.invalid",
			Port:    443,
		}, &model.DestinationService{
			Type:     "external",
			Name:     "https://testing.invalid",
			Resource: "testing.invalid:443",
		}, map[string]pdata.AttributeValue{
			"http.url": pdata.NewAttributeValueString("https://testing.invalid/foo?bar"),
		})
	})
	t.Run("url_non_default_port", func(t *testing.T) {
		test(t, model.Destination{
			Address: "testing.invalid",
			Port:    444,
		}, &model.DestinationService{
			Type:     "external",
			Name:     "https://testing.invalid:444",
			Resource: "testing.invalid:444",
		}, map[string]pdata.AttributeValue{
			"http.url": pdata.NewAttributeValueString("https://testing.invalid:444/foo?bar"),
		})
	})
	t.Run("scheme_host_target", func(t *testing.T) {
		test(t, model.Destination{
			Address: "testing.invalid",
			Port:    444,
		}, &model.DestinationService{
			Type:     "external",
			Name:     "https://testing.invalid:444",
			Resource: "testing.invalid:444",
		}, map[string]pdata.AttributeValue{
			"http.scheme": pdata.NewAttributeValueString("https"),
			"http.host":   pdata.NewAttributeValueString("testing.invalid:444"),
			"http.target": pdata.NewAttributeValueString("/foo?bar"),
		})
	})
	t.Run("scheme_netpeername_nethostport_target", func(t *testing.T) {
		test(t, model.Destination{
			Address: "::1",
			Port:    444,
		}, &model.DestinationService{
			Type:     "external",
			Name:     "https://[::1]:444",
			Resource: "[::1]:444",
		}, map[string]pdata.AttributeValue{
			"http.scheme":   pdata.NewAttributeValueString("https"),
			"net.peer.ip":   pdata.NewAttributeValueString("::1"),
			"net.peer.port": pdata.NewAttributeValueInt(444),
			"http.target":   pdata.NewAttributeValueString("/foo?bar"),
		})
	})
}

func TestHTTPTransactionSource(t *testing.T) {
	test := func(t *testing.T, expectedDomain, expectedIP string, expectedPort int, attrs map[string]pdata.AttributeValue) {
		// "http.method" is a required attribute for HTTP spans,
		// and its presence causes the transaction's HTTP request
		// context to be built.
		attrs["http.method"] = pdata.NewAttributeValueString("POST")

		event := transformTransactionWithAttributes(t, attrs)
		require.NotNil(t, event.HTTP)
		require.NotNil(t, event.HTTP.Request)
		parsedIP := net.ParseIP(expectedIP)
		require.NotNil(t, parsedIP)
		assert.Equal(t, model.Source{
			Domain: expectedDomain,
			IP:     net.ParseIP(expectedIP),
			Port:   expectedPort,
		}, event.Source)
		assert.Equal(t, model.Client(event.Source), event.Client)
	}

	t.Run("net.peer.ip_port", func(t *testing.T) {
		test(t, "", "192.168.0.1", 1234, map[string]pdata.AttributeValue{
			"net.peer.ip":   pdata.NewAttributeValueString("192.168.0.1"),
			"net.peer.port": pdata.NewAttributeValueInt(1234),
		})
	})
	t.Run("net.peer.ip", func(t *testing.T) {
		test(t, "", "192.168.0.1", 0, map[string]pdata.AttributeValue{
			"net.peer.ip": pdata.NewAttributeValueString("192.168.0.1"),
		})
	})
	t.Run("net.peer.ip_name", func(t *testing.T) {
		test(t, "source.domain", "192.168.0.1", 0, map[string]pdata.AttributeValue{
			"net.peer.name": pdata.NewAttributeValueString("source.domain"),
			"net.peer.ip":   pdata.NewAttributeValueString("192.168.0.1"),
		})
	})
}

func TestHTTPTransactionFlavor(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"http.flavor": pdata.NewAttributeValueString("1.1"),
	})
	assert.Equal(t, "1.1", event.HTTP.Version)
}

func TestHTTPTransactionUserAgent(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"http.user_agent": pdata.NewAttributeValueString("Foo/bar (baz)"),
	})
	assert.Equal(t, model.UserAgent{Original: "Foo/bar (baz)"}, event.UserAgent)
}

func TestHTTPTransactionClientIP(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"net.peer.ip":    pdata.NewAttributeValueString("1.2.3.4"),
		"net.peer.port":  pdata.NewAttributeValueInt(5678),
		"http.client_ip": pdata.NewAttributeValueString("9.10.11.12"),
	})
	assert.Equal(t, model.Source{IP: net.ParseIP("1.2.3.4"), Port: 5678}, event.Source)
	assert.Equal(t, model.Client{IP: net.ParseIP("9.10.11.12")}, event.Client)
}

func TestHTTPTransactionStatusCode(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"http.status_code": pdata.NewAttributeValueInt(200),
	})
	assert.Equal(t, 200, event.HTTP.Response.StatusCode)
}

func TestDatabaseSpan(t *testing.T) {
	// https://github.com/open-telemetry/opentelemetry-specification/blob/master/specification/trace/semantic_conventions/database.md#mysql
	connectionString := "Server=shopdb.example.com;Database=ShopDb;Uid=billing_user;TableCache=true;UseCompression=True;MinimumPoolSize=10;MaximumPoolSize=50;"
	dbStatement := fmt.Sprintf("SELECT * FROM orders WHERE order_id = '%s'", strings.Repeat("*", 1024)) // should not be truncated!
	event := transformSpanWithAttributes(t, map[string]pdata.AttributeValue{
		"db.system":            pdata.NewAttributeValueString("mysql"),
		"db.connection_string": pdata.NewAttributeValueString(connectionString),
		"db.user":              pdata.NewAttributeValueString("billing_user"),
		"db.name":              pdata.NewAttributeValueString("ShopDb"),
		"db.statement":         pdata.NewAttributeValueString(dbStatement),
		"net.peer.name":        pdata.NewAttributeValueString("shopdb.example.com"),
		"net.peer.ip":          pdata.NewAttributeValueString("192.0.2.12"),
		"net.peer.port":        pdata.NewAttributeValueInt(3306),
		"net.transport":        pdata.NewAttributeValueString("IP.TCP"),
	})

	assert.Equal(t, "db", event.Span.Type)
	assert.Equal(t, "mysql", event.Span.Subtype)
	assert.Equal(t, "", event.Span.Action)

	assert.Equal(t, &model.DB{
		Instance:  "ShopDb",
		Statement: dbStatement,
		Type:      "mysql",
		UserName:  "billing_user",
	}, event.Span.DB)

	assert.Equal(t, model.Labels{
		"db_connection_string": {Value: connectionString},
		"net_transport":        {Value: "IP.TCP"},
	}, event.Labels)

	assert.Equal(t, model.Destination{
		Address: "shopdb.example.com",
		Port:    3306,
	}, event.Destination)

	assert.Equal(t, &model.DestinationService{
		Type:     "db",
		Name:     "mysql",
		Resource: "mysql",
	}, event.Span.DestinationService)
}

func TestInstrumentationLibrary(t *testing.T) {
	traces, spans := newTracesSpans()
	spans.InstrumentationLibrary().SetName("library-name")
	spans.InstrumentationLibrary().SetVersion("1.2.3")
	otelSpan := spans.Spans().AppendEmpty()
	otelSpan.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan.SetSpanID(pdata.NewSpanID([8]byte{2}))
	events := transformTraces(t, traces)
	event := events[0]

	assert.Equal(t, "library-name", event.Service.Framework.Name)
	assert.Equal(t, "1.2.3", event.Service.Framework.Version)
}

func TestRPCTransaction(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"rpc.system":           pdata.NewAttributeValueString("grpc"),
		"rpc.service":          pdata.NewAttributeValueString("myservice.EchoService"),
		"rpc.method":           pdata.NewAttributeValueString("exampleMethod"),
		"rpc.grpc.status_code": pdata.NewAttributeValueInt(int64(codes.Unavailable)),
		"net.peer.name":        pdata.NewAttributeValueString("peer_name"),
		"net.peer.ip":          pdata.NewAttributeValueString("10.20.30.40"),
		"net.peer.port":        pdata.NewAttributeValueInt(123),
	})
	assert.Equal(t, "request", event.Transaction.Type)
	assert.Equal(t, "Unavailable", event.Transaction.Result)
	assert.Empty(t, event.Labels)
	assert.Equal(t, model.Client{
		Domain: "peer_name",
		IP:     net.ParseIP("10.20.30.40"),
		Port:   123,
	}, event.Client)
}

func TestRPCSpan(t *testing.T) {
	event := transformSpanWithAttributes(t, map[string]pdata.AttributeValue{
		"rpc.system":           pdata.NewAttributeValueString("grpc"),
		"rpc.service":          pdata.NewAttributeValueString("myservice.EchoService"),
		"rpc.method":           pdata.NewAttributeValueString("exampleMethod"),
		"rpc.grpc.status_code": pdata.NewAttributeValueInt(int64(codes.Unavailable)),
		"net.peer.ip":          pdata.NewAttributeValueString("10.20.30.40"),
		"net.peer.port":        pdata.NewAttributeValueInt(123),
	})
	assert.Equal(t, "external", event.Span.Type)
	assert.Equal(t, "grpc", event.Span.Subtype)
	assert.Empty(t, event.Labels)
	assert.Equal(t, model.Destination{
		Address: "10.20.30.40",
		Port:    123,
	}, event.Destination)
	assert.Equal(t, &model.DestinationService{
		Type:     "external",
		Name:     "10.20.30.40:123",
		Resource: "10.20.30.40:123",
	}, event.Span.DestinationService)
}

func TestMessagingTransaction(t *testing.T) {
	event := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"messaging.destination": pdata.NewAttributeValueString("myQueue"),
	}, func(s pdata.Span) {
		s.SetKind(pdata.SpanKindConsumer)
		// Set parentID to imply this isn't the root, but
		// kind==Consumer should still force the span to be translated
		// as a transaction.
		s.SetParentSpanID(pdata.NewSpanID([8]byte{3}))
	})
	assert.Equal(t, "messaging", event.Transaction.Type)
	assert.Empty(t, event.Labels)
	assert.Equal(t, &model.Message{
		QueueName: "myQueue",
	}, event.Transaction.Message)
}

func TestMessagingSpan(t *testing.T) {
	event := transformSpanWithAttributes(t, map[string]pdata.AttributeValue{
		"messaging.system":      pdata.NewAttributeValueString("kafka"),
		"messaging.destination": pdata.NewAttributeValueString("myTopic"),
		"net.peer.ip":           pdata.NewAttributeValueString("10.20.30.40"),
		"net.peer.port":         pdata.NewAttributeValueInt(123),
	}, func(s pdata.Span) {
		s.SetKind(pdata.SpanKindProducer)
	})
	assert.Equal(t, "messaging", event.Span.Type)
	assert.Equal(t, "kafka", event.Span.Subtype)
	assert.Equal(t, "send", event.Span.Action)
	assert.Empty(t, event.Labels)
	assert.Equal(t, model.Destination{
		Address: "10.20.30.40",
		Port:    123,
	}, event.Destination)
	assert.Equal(t, &model.DestinationService{
		Type:     "messaging",
		Name:     "kafka",
		Resource: "kafka/myTopic",
	}, event.Span.DestinationService)
}

func TestSpanType(t *testing.T) {
	// Internal spans default to app.internal.
	event := transformSpanWithAttributes(t, map[string]pdata.AttributeValue{}, func(s pdata.Span) {
		s.SetKind(pdata.SpanKindInternal)
	})
	assert.Equal(t, "app", event.Span.Type)
	assert.Equal(t, "internal", event.Span.Subtype)

	// All other spans default to unknown.
	event = transformSpanWithAttributes(t, map[string]pdata.AttributeValue{}, func(s pdata.Span) {
		s.SetKind(pdata.SpanKindClient)
	})
	assert.Equal(t, "unknown", event.Span.Type)
	assert.Equal(t, "", event.Span.Subtype)
}

func TestSpanNetworkAttributes(t *testing.T) {
	networkAttributes := map[string]pdata.AttributeValue{
		"net.host.connection.type":    pdata.NewAttributeValueString("cell"),
		"net.host.connection.subtype": pdata.NewAttributeValueString("LTE"),
		"net.host.carrier.name":       pdata.NewAttributeValueString("Vodafone"),
		"net.host.carrier.mnc":        pdata.NewAttributeValueString("01"),
		"net.host.carrier.mcc":        pdata.NewAttributeValueString("101"),
		"net.host.carrier.icc":        pdata.NewAttributeValueString("UK"),
	}
	txEvent := transformTransactionWithAttributes(t, networkAttributes)
	spanEvent := transformSpanWithAttributes(t, networkAttributes)

	expected := model.Network{
		Connection: model.NetworkConnection{
			Type:    "cell",
			Subtype: "LTE",
		},
		Carrier: model.NetworkCarrier{
			Name: "Vodafone",
			MNC:  "01",
			MCC:  "101",
			ICC:  "UK",
		},
	}
	assert.Equal(t, expected, txEvent.Network)
	assert.Equal(t, expected, spanEvent.Network)
}

func TestArrayLabels(t *testing.T) {
	stringArray := pdata.NewAttributeValueArray()
	stringArray.ArrayVal().AppendEmpty().SetStringVal("string1")
	stringArray.ArrayVal().AppendEmpty().SetStringVal("string2")

	boolArray := pdata.NewAttributeValueArray()
	boolArray.ArrayVal().AppendEmpty().SetBoolVal(false)
	boolArray.ArrayVal().AppendEmpty().SetBoolVal(true)

	intArray := pdata.NewAttributeValueArray()
	intArray.ArrayVal().AppendEmpty().SetIntVal(1234)
	intArray.ArrayVal().AppendEmpty().SetIntVal(5678)

	floatArray := pdata.NewAttributeValueArray()
	floatArray.ArrayVal().AppendEmpty().SetDoubleVal(1234.5678)
	floatArray.ArrayVal().AppendEmpty().SetDoubleVal(9123.234123123)

	txEvent := transformTransactionWithAttributes(t, map[string]pdata.AttributeValue{
		"string_array": stringArray,
		"bool_array":   boolArray,
		"int_array":    intArray,
		"float_array":  floatArray,
	})
	assert.Equal(t, model.Labels{
		"bool_array":   {Values: []string{"false", "true"}},
		"string_array": {Values: []string{"string1", "string2"}},
	}, txEvent.Labels)
	assert.Equal(t, model.NumericLabels{
		"int_array":   {Values: []float64{1234, 5678}},
		"float_array": {Values: []float64{1234.5678, 9123.234123123}},
	}, txEvent.NumericLabels)

	spanEvent := transformSpanWithAttributes(t, map[string]pdata.AttributeValue{
		"string_array": stringArray,
		"bool_array":   boolArray,
		"int_array":    intArray,
		"float_array":  floatArray,
	})
	assert.Equal(t, model.Labels{
		"bool_array":   {Values: []string{"false", "true"}},
		"string_array": {Values: []string{"string1", "string2"}},
	}, spanEvent.Labels)
	assert.Equal(t, model.NumericLabels{
		"int_array":   {Values: []float64{1234, 5678}},
		"float_array": {Values: []float64{1234.5678, 9123.234123123}},
	}, spanEvent.NumericLabels)
}

func TestConsumeTracesExportTimestamp(t *testing.T) {
	traces, otelSpans := newTracesSpans()

	// The actual timestamps will be non-deterministic, as they are adjusted
	// based on the server's clock.
	//
	// Use a large delta so that we can allow for a significant amount of
	// delay in the test environment affecting the timestamp adjustment.
	const timeDelta = time.Hour
	const allowedError = 5 // seconds

	now := time.Now()
	exportTimestamp := now.Add(-timeDelta)
	traces.ResourceSpans().At(0).Resource().Attributes().InitFromMap(map[string]pdata.AttributeValue{
		"telemetry.sdk.elastic_export_timestamp": pdata.NewAttributeValueInt(exportTimestamp.UnixNano()),
	})

	// Offsets are start times relative to the export timestamp.
	transactionOffset := -2 * time.Second
	spanOffset := transactionOffset + time.Second
	exceptionOffset := spanOffset + 25*time.Millisecond
	transactionDuration := time.Second + 100*time.Millisecond
	spanDuration := 50 * time.Millisecond

	exportedTransactionTimestamp := exportTimestamp.Add(transactionOffset)
	exportedSpanTimestamp := exportTimestamp.Add(spanOffset)
	exportedExceptionTimestamp := exportTimestamp.Add(exceptionOffset)

	otelSpan1 := otelSpans.Spans().AppendEmpty()
	otelSpan1.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan1.SetSpanID(pdata.NewSpanID([8]byte{2}))
	otelSpan1.SetStartTimestamp(pdata.TimestampFromTime(exportedTransactionTimestamp))
	otelSpan1.SetEndTimestamp(pdata.TimestampFromTime(exportedTransactionTimestamp.Add(transactionDuration)))

	otelSpan2 := otelSpans.Spans().AppendEmpty()
	otelSpan2.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan2.SetSpanID(pdata.NewSpanID([8]byte{2}))
	otelSpan2.SetParentSpanID(pdata.NewSpanID([8]byte{3}))
	otelSpan2.SetStartTimestamp(pdata.TimestampFromTime(exportedSpanTimestamp))
	otelSpan2.SetEndTimestamp(pdata.TimestampFromTime(exportedSpanTimestamp.Add(spanDuration)))

	otelSpanEvent := otelSpan2.Events().AppendEmpty()
	otelSpanEvent.SetTimestamp(pdata.TimestampFromTime(exportedExceptionTimestamp))
	otelSpanEvent.SetName("exception")
	otelSpanEvent.Attributes().InitFromMap(map[string]pdata.AttributeValue{
		"exception.type":       pdata.NewAttributeValueString("the_type"),
		"exception.message":    pdata.NewAttributeValueString("the_message"),
		"exception.stacktrace": pdata.NewAttributeValueString("the_stacktrace"),
	})

	batch := transformTraces(t, traces)
	require.Len(t, batch, 3)

	// Give some leeway for one event, and check other events' timestamps relative to that one.
	assert.InDelta(t, now.Add(transactionOffset).Unix(), batch[0].Timestamp.Unix(), allowedError)
	assert.Equal(t, spanOffset-transactionOffset, batch[1].Timestamp.Sub(batch[0].Timestamp))
	assert.Equal(t, exceptionOffset-transactionOffset, batch[2].Timestamp.Sub(batch[0].Timestamp))

	// Durations should be unaffected.
	assert.Equal(t, transactionDuration, batch[0].Event.Duration)
	assert.Equal(t, spanDuration, batch[1].Event.Duration)
}

func TestConsumer_JaegerMetadata(t *testing.T) {
	jaegerBatch := jaegermodel.Batch{
		Spans: []*jaegermodel.Span{{
			StartTime: testStartTime(),
			Tags:      []jaegermodel.KeyValue{jaegerKeyValue("span.kind", "client")},
			TraceID:   jaegermodel.NewTraceID(0, 0x46467830),
			SpanID:    jaegermodel.NewSpanID(0x41414646),
		}},
	}

	for _, tc := range []struct {
		name    string
		process *jaegermodel.Process
	}{{
		name: "jaeger-version",
		process: jaegermodel.NewProcess("", []jaegermodel.KeyValue{
			jaegerKeyValue("jaeger.version", "PHP-3.4.12"),
		}),
	}, {
		name: "jaeger-no-language",
		process: jaegermodel.NewProcess("", []jaegermodel.KeyValue{
			jaegerKeyValue("jaeger.version", "3.4.12"),
		}),
	}, {
		// TODO(axw) break this down into more specific test cases.
		name: "jaeger",
		process: jaegermodel.NewProcess("foo", []jaegermodel.KeyValue{
			jaegerKeyValue("jaeger.version", "C++-3.2.1"),
			jaegerKeyValue("hostname", "host-foo"),
			jaegerKeyValue("client-uuid", "xxf0"),
			jaegerKeyValue("ip", "17.0.10.123"),
			jaegerKeyValue("foo", "bar"),
			jaegerKeyValue("peer.port", "80"),
		}),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var events []beat.Event
			recorder := eventRecorderBatchProcessor(&events)
			jaegerBatch.Process = tc.process
			traces := jaegertranslator.ProtoBatchToInternalTraces(jaegerBatch)
			require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))
			approveEvents(t, "metadata_"+tc.name, events)
		})
	}
}

func TestConsumer_JaegerSampleRate(t *testing.T) {
	jaegerBatch := jaegermodel.Batch{
		Process: jaegermodel.NewProcess("", jaegerKeyValues(
			"jaeger.version", "unknown",
			"hostname", "host-abc",
		)),
		Spans: []*jaegermodel.Span{{
			StartTime: testStartTime(),
			Duration:  testDuration(),
			Tags: []jaegermodel.KeyValue{
				jaegerKeyValue("span.kind", "server"),
				jaegerKeyValue("sampler.type", "probabilistic"),
				jaegerKeyValue("sampler.param", 0.8),
			},
		}, {
			StartTime: testStartTime(),
			Duration:  testDuration(),
			TraceID:   jaegermodel.NewTraceID(1, 1),
			References: []jaegermodel.SpanRef{{
				RefType: jaegermodel.SpanRefType_CHILD_OF,
				TraceID: jaegermodel.NewTraceID(1, 1),
				SpanID:  1,
			}},
			Tags: []jaegermodel.KeyValue{
				jaegerKeyValue("span.kind", "client"),
				jaegerKeyValue("sampler.type", "probabilistic"),
				jaegerKeyValue("sampler.param", 0.4),
			},
		}, {
			StartTime: testStartTime(),
			Duration:  testDuration(),
			Tags: []jaegermodel.KeyValue{
				jaegerKeyValue("span.kind", "server"),
				jaegerKeyValue("sampler.type", "ratelimiting"),
				jaegerKeyValue("sampler.param", 2.0), // 2 traces per second
			},
		}},
	}
	traces := jaegertranslator.ProtoBatchToInternalTraces(jaegerBatch)

	var batches []*model.Batch
	recorder := batchRecorderBatchProcessor(&batches)
	require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))
	require.Len(t, batches, 1)
	batch := *batches[0]

	events := transformBatch(context.Background(), batches...)
	approveEvents(t, "jaeger_sampling_rate", events)

	tx1 := batch[0].Transaction
	span := batch[1].Span
	tx2 := batch[2].Transaction
	assert.Equal(t, 1.25 /* 1/0.8 */, tx1.RepresentativeCount)
	assert.Equal(t, 2.5 /* 1/0.4 */, span.RepresentativeCount)
	assert.Zero(t, tx2.RepresentativeCount) // not set for non-probabilistic
}

func TestConsumer_JaegerTraceID(t *testing.T) {
	var batches []*model.Batch
	recorder := batchRecorderBatchProcessor(&batches)

	jaegerBatch := jaegermodel.Batch{
		Process: jaegermodel.NewProcess("", jaegerKeyValues("jaeger.version", "unknown")),
		Spans: []*jaegermodel.Span{{
			TraceID: jaegermodel.NewTraceID(0, 0x000046467830),
			SpanID:  jaegermodel.NewSpanID(456),
		}, {
			TraceID: jaegermodel.NewTraceID(0x000046467830, 0x000046467830),
			SpanID:  jaegermodel.NewSpanID(789),
		}},
	}
	traces := jaegertranslator.ProtoBatchToInternalTraces(jaegerBatch)
	require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))

	batch := *batches[0]
	assert.Equal(t, "00000000000000000000000046467830", batch[0].Trace.ID)
	assert.Equal(t, "00000000464678300000000046467830", batch[1].Trace.ID)
}

func TestConsumer_JaegerTransaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spans []*jaegermodel.Span
	}{
		{
			name: "jaeger_full",
			spans: []*jaegermodel.Span{{
				StartTime:     testStartTime(),
				Duration:      testDuration(),
				TraceID:       jaegermodel.NewTraceID(0, 0x46467830),
				SpanID:        0x41414646,
				OperationName: "HTTP GET",
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("error", true),
					jaegerKeyValue("bool.a", true),
					jaegerKeyValue("double.a", 14.65),
					jaegerKeyValue("int.a", int64(148)),
					jaegerKeyValue("http.method", "get"),
					jaegerKeyValue("http.url", "http://foo.bar.com?a=12"),
					jaegerKeyValue("http.status_code", "400"),
					jaegerKeyValue("http.protocol", "HTTP/1.1"),
					jaegerKeyValue("type", "http_request"),
					jaegerKeyValue("component", "foo"),
					jaegerKeyValue("string.a.b", "some note"),
					jaegerKeyValue("service.version", "1.0"),
				},
				Logs: testJaegerLogs(),
			}},
		},
		{
			name: "jaeger_type_request",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				References: []jaegermodel.SpanRef{{
					RefType: jaegermodel.SpanRefType_CHILD_OF,
					SpanID:  0x61626364,
				}},
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("span.kind", "server"),
					jaegerKeyValue("http.status_code", int64(500)),
					jaegerKeyValue("http.protocol", "HTTP"),
					jaegerKeyValue("http.path", "http://foo.bar.com?a=12"),
				},
			}},
		},
		{
			name: "jaeger_type_request_result",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				References: []jaegermodel.SpanRef{{
					RefType: jaegermodel.SpanRefType_CHILD_OF,
					SpanID:  0x61626364,
				}},
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("span.kind", "server"),
					jaegerKeyValue("http.status_code", int64(200)),
					jaegerKeyValue("http.url", "localhost:8080"),
				},
			}},
		},
		{
			name: "jaeger_type_messaging",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				References: []jaegermodel.SpanRef{{
					RefType: jaegermodel.SpanRefType_CHILD_OF,
					SpanID:  0x61626364,
				}},
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("span.kind", "server"),
					jaegerKeyValue("message_bus.destination", "queue-abc"),
				},
			}},
		},
		{
			name: "jaeger_type_component",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("component", "amqp"),
				},
			}},
		},
		{
			name: "jaeger_custom",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("a.b", "foo"),
				},
			}},
		},
		{
			name: "jaeger_no_attrs",
			spans: []*jaegermodel.Span{{
				StartTime: testStartTime(),
				Duration:  testDuration(),
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("span.kind", "server"),
					jaegerKeyValue("error", true),
					jaegerKeyValue("otel.status_code", int64(2)),
				},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := jaegermodel.Batch{
				Process: jaegermodel.NewProcess("", []jaegermodel.KeyValue{
					jaegerKeyValue("hostname", "host-abc"),
					jaegerKeyValue("jaeger.version", "unknown"),
				}),
				Spans: tc.spans,
			}
			traces := jaegertranslator.ProtoBatchToInternalTraces(batch)

			var events []beat.Event
			recorder := eventRecorderBatchProcessor(&events)
			require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))
			approveEvents(t, "transaction_"+tc.name, events)
		})
	}
}

func TestConsumer_JaegerSpan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spans []*jaegermodel.Span
	}{
		{
			name: "jaeger_http",
			spans: []*jaegermodel.Span{{
				OperationName: "HTTP GET",
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("error", true),
					jaegerKeyValue("hasErrors", true),
					jaegerKeyValue("double.a", 14.65),
					jaegerKeyValue("http.status_code", int64(400)),
					jaegerKeyValue("int.a", int64(148)),
					jaegerKeyValue("span.kind", "filtered"),
					jaegerKeyValue("http.url", "http://foo.bar.com?a=12"),
					jaegerKeyValue("http.method", "get"),
					jaegerKeyValue("component", "foo"),
					jaegerKeyValue("string.a.b", "some note"),
				},
				Logs: testJaegerLogs(),
			}},
		},
		{
			name: "jaeger_https_default_port",
			spans: []*jaegermodel.Span{{
				OperationName: "HTTPS GET",
				Tags: jaegerKeyValues(
					"http.url", "https://foo.bar.com:443?a=12",
				),
			}},
		},
		{
			name: "jaeger_http_status_code",
			spans: []*jaegermodel.Span{{
				OperationName: "HTTP GET",
				Tags: jaegerKeyValues(
					"http.url", "http://foo.bar.com?a=12",
					"http.method", "get",
					"http.status_code", int64(202),
				),
			}},
		},
		{
			name: "jaeger_db",
			spans: []*jaegermodel.Span{{
				Tags: jaegerKeyValues(
					"db.statement", "GET * from users",
					"db.instance", "db01",
					"db.type", "mysql",
					"db.user", "admin",
					"component", "foo",
					"peer.address", "mysql://db:3306",
					"peer.hostname", "db",
					"peer.port", int64(3306),
					"peer.service", "sql",
				),
			}},
		},
		{
			name: "jaeger_messaging",
			spans: []*jaegermodel.Span{{
				OperationName: "Message receive",
				Tags: jaegerKeyValues(
					"peer.hostname", "mq",
					"peer.port", int64(1234),
					"message_bus.destination", "queue-abc",
				),
			}},
		},
		{
			name: "jaeger_subtype_component",
			spans: []*jaegermodel.Span{{
				Tags: []jaegermodel.KeyValue{
					jaegerKeyValue("component", "whatever"),
				},
			}},
		},
		{
			name:  "jaeger_custom",
			spans: []*jaegermodel.Span{{}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := jaegermodel.Batch{
				Process: jaegermodel.NewProcess("", []jaegermodel.KeyValue{
					jaegerKeyValue("hostname", "host-abc"),
					jaegerKeyValue("jaeger.version", "unknown"),
				}),
				Spans: tc.spans,
			}
			for _, span := range batch.Spans {
				span.StartTime = testStartTime()
				span.Duration = testDuration()
				span.TraceID = jaegermodel.NewTraceID(0, 0x46467830)
				span.SpanID = 0x41414646
				span.References = []jaegermodel.SpanRef{{
					RefType: jaegermodel.SpanRefType_CHILD_OF,
					TraceID: jaegermodel.NewTraceID(0, 0x46467830),
					SpanID:  0x58585858,
				}}
			}
			traces := jaegertranslator.ProtoBatchToInternalTraces(batch)

			var events []beat.Event
			recorder := eventRecorderBatchProcessor(&events)
			require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))
			approveEvents(t, "span_"+tc.name, events)
		})
	}
}

func TestJaegerServiceVersion(t *testing.T) {
	jaegerBatch := jaegermodel.Batch{
		Process: jaegermodel.NewProcess("", jaegerKeyValues(
			"jaeger.version", "unknown",
			"service.version", "process_tag_value",
		)),
		Spans: []*jaegermodel.Span{{
			TraceID: jaegermodel.NewTraceID(0, 0x000046467830),
			SpanID:  jaegermodel.NewSpanID(456),
		}, {
			TraceID: jaegermodel.NewTraceID(0, 0x000046467830),
			SpanID:  jaegermodel.NewSpanID(456),
			Tags: []jaegermodel.KeyValue{
				jaegerKeyValue("service.version", "span_tag_value"),
			},
		}},
	}
	traces := jaegertranslator.ProtoBatchToInternalTraces(jaegerBatch)

	var batches []*model.Batch
	recorder := batchRecorderBatchProcessor(&batches)
	require.NoError(t, (&otel.Consumer{Processor: recorder}).ConsumeTraces(context.Background(), traces))

	batch := *batches[0]
	assert.Equal(t, "process_tag_value", batch[0].Service.Version)
	assert.Equal(t, "span_tag_value", batch[1].Service.Version)
}

func TestTracesLogging(t *testing.T) {
	for _, level := range []logp.Level{logp.InfoLevel, logp.DebugLevel} {
		t.Run(level.String(), func(t *testing.T) {
			logp.DevelopmentSetup(logp.ToObserverOutput(), logp.WithLevel(level))
			transformTraces(t, pdata.NewTraces())
			logs := logp.ObserverLogs().TakeAll()
			if level == logp.InfoLevel {
				assert.Empty(t, logs)
			} else {
				assert.NotEmpty(t, logs)
			}
		})
	}
}

func testJaegerLogs() []jaegermodel.Log {
	return []jaegermodel.Log{{
		// errors that can be converted to elastic errors
		Timestamp: testStartTime().Add(23 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"event", "retrying connection",
			"level", "error",
			"error", "no connection established",
		),
	}, {
		Timestamp: testStartTime().Add(43 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"event", "no user.ID given",
			"level", "error",
			"message", "nullPointer exception",
			"isbool", true,
		),
	}, {
		Timestamp: testStartTime().Add(66 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"error", "no connection established",
		),
	}, {
		Timestamp: testStartTime().Add(66 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"error.object", "no connection established",
		),
	}, {
		Timestamp: testStartTime().Add(66 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"error.kind", "DBClosedException",
		),
	}, {
		Timestamp: testStartTime().Add(66 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"event", "error",
			"message", "no connection established",
		),
	}, {
		// non errors
		Timestamp: testStartTime().Add(15 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"event", "baggage",
			"isValid", false,
		),
	}, {
		Timestamp: testStartTime().Add(65 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"message", "retrying connection",
			"level", "info",
		),
	}, {
		// errors not convertible to elastic errors
		Timestamp: testStartTime().Add(67 * time.Nanosecond),
		Fields: jaegerKeyValues(
			"level", "error",
		),
	}}
}

func testStartTime() time.Time {
	return time.Unix(1576500418, 768068)
}

func testDuration() time.Duration {
	return 79 * time.Second
}

func batchRecorderBatchProcessor(out *[]*model.Batch) model.BatchProcessor {
	return model.ProcessBatchFunc(func(ctx context.Context, batch *model.Batch) error {
		*out = append(*out, batch)
		return nil
	})
}

func eventRecorderBatchProcessor(out *[]beat.Event) model.BatchProcessor {
	return model.ProcessBatchFunc(func(ctx context.Context, batch *model.Batch) error {
		*out = append(*out, transformBatch(ctx, batch)...)
		return nil
	})
}

func transformBatch(ctx context.Context, batches ...*model.Batch) []beat.Event {
	var out []beat.Event
	for _, batch := range batches {
		out = append(out, batch.Transform(ctx)...)
	}
	return out
}

func approveEvents(t testing.TB, name string, events []beat.Event) {
	t.Helper()
	docs := beatertest.EncodeEventDocs(events...)
	approvaltest.ApproveEventDocs(t, filepath.Join("test_approved", name), docs)
}

func jaegerKeyValues(kv ...interface{}) []jaegermodel.KeyValue {
	if len(kv)%2 != 0 {
		panic("even number of args expected")
	}
	out := make([]jaegermodel.KeyValue, len(kv)/2)
	for i := range out {
		k := kv[2*i].(string)
		v := kv[2*i+1]
		out[i] = jaegerKeyValue(k, v)
	}
	return out
}

func jaegerKeyValue(k string, v interface{}) jaegermodel.KeyValue {
	kv := jaegermodel.KeyValue{Key: k}
	switch v := v.(type) {
	case string:
		kv.VType = jaegermodel.ValueType_STRING
		kv.VStr = v
	case float64:
		kv.VType = jaegermodel.ValueType_FLOAT64
		kv.VFloat64 = v
	case int64:
		kv.VType = jaegermodel.ValueType_INT64
		kv.VInt64 = v
	case bool:
		kv.VType = jaegermodel.ValueType_BOOL
		kv.VBool = v
	default:
		panic(fmt.Errorf("unhandled %q value type %#v", k, v))
	}
	return kv
}

func transformTransactionWithAttributes(t *testing.T, attrs map[string]pdata.AttributeValue, configFns ...func(pdata.Span)) model.APMEvent {
	traces, spans := newTracesSpans()
	otelSpan := spans.Spans().AppendEmpty()
	otelSpan.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan.SetSpanID(pdata.NewSpanID([8]byte{2}))
	for _, fn := range configFns {
		fn(otelSpan)
	}
	otelSpan.Attributes().InitFromMap(attrs)
	events := transformTraces(t, traces)
	return events[0]
}

func transformSpanWithAttributes(t *testing.T, attrs map[string]pdata.AttributeValue, configFns ...func(pdata.Span)) model.APMEvent {
	traces, spans := newTracesSpans()
	otelSpan := spans.Spans().AppendEmpty()
	otelSpan.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan.SetSpanID(pdata.NewSpanID([8]byte{2}))
	otelSpan.SetParentSpanID(pdata.NewSpanID([8]byte{3}))
	for _, fn := range configFns {
		fn(otelSpan)
	}
	otelSpan.Attributes().InitFromMap(attrs)
	events := transformTraces(t, traces)
	return events[0]
}

func transformTransactionSpanEvents(t *testing.T, language string, spanEvents ...pdata.SpanEvent) (transaction model.APMEvent, events []model.APMEvent) {
	traces, spans := newTracesSpans()
	traces.ResourceSpans().At(0).Resource().Attributes().InitFromMap(map[string]pdata.AttributeValue{
		semconv.AttributeTelemetrySDKLanguage: pdata.NewAttributeValueString(language),
	})
	otelSpan := spans.Spans().AppendEmpty()
	otelSpan.SetTraceID(pdata.NewTraceID([16]byte{1}))
	otelSpan.SetSpanID(pdata.NewSpanID([8]byte{2}))
	for _, spanEvent := range spanEvents {
		spanEvent.CopyTo(otelSpan.Events().AppendEmpty())
	}

	allEvents := transformTraces(t, traces)
	require.NotEmpty(t, allEvents)
	return allEvents[0], allEvents[1:]
}

func transformTraces(t *testing.T, traces pdata.Traces) model.Batch {
	var processed model.Batch
	processor := model.ProcessBatchFunc(func(ctx context.Context, batch *model.Batch) error {
		if processed != nil {
			panic("already processes batch")
		}
		processed = *batch
		return nil
	})
	require.NoError(t, (&otel.Consumer{Processor: processor}).ConsumeTraces(context.Background(), traces))
	return processed
}

func newTracesSpans() (pdata.Traces, pdata.InstrumentationLibrarySpans) {
	traces := pdata.NewTraces()
	resourceSpans := traces.ResourceSpans().AppendEmpty()
	librarySpans := resourceSpans.InstrumentationLibrarySpans().AppendEmpty()
	return traces, librarySpans
}

func newInt(v int) *int {
	return &v
}

func newBool(v bool) *bool {
	return &v
}
