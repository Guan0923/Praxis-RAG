package main

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDocumentWorkerMode(t *testing.T) {
	cases := []struct {
		name            string
		raw             string
		kafkaConfigured bool
		want            bool
		wantErr         string
	}{
		{name: "auto without kafka embeds the worker", raw: "", kafkaConfigured: false, want: true},
		{name: "explicit auto without kafka embeds the worker", raw: "auto", kafkaConfigured: false, want: true},
		{name: "auto with kafka leaves the worker external", raw: "auto", kafkaConfigured: true, want: false},
		{name: "on without kafka embeds the worker", raw: "on", kafkaConfigured: false, want: true},
		{
			name: "on with kafka fails", raw: "on", kafkaConfigured: true,
			wantErr: "RAG_DOCUMENT_WORKER_INPROCESS requires RAG_KAFKA_BOOTSTRAP_SERVERS to be empty; run rag-document-worker instead",
		},
		{name: "true acts like on", raw: " TRUE ", kafkaConfigured: false, want: true},
		{name: "off without kafka stays external", raw: "off", kafkaConfigured: false, want: false},
		{name: "off with kafka stays external", raw: "off", kafkaConfigured: true, want: false},
		{name: "zero acts like off", raw: "0", kafkaConfigured: false, want: false},
		{name: "invalid value fails", raw: "maybe", kafkaConfigured: false, wantErr: "RAG_DOCUMENT_WORKER_INPROCESS must be auto, on or off"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			enabled, err := resolveDocumentWorkerMode(testCase.raw, testCase.kafkaConfigured)
			if testCase.wantErr != "" {
				if err == nil || err.Error() != testCase.wantErr {
					t.Fatalf("error = %v, want %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if enabled != testCase.want {
				t.Fatalf("enabled = %v, want %v", enabled, testCase.want)
			}
		})
	}
}

func TestDocumentWorkerPollInterval(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", raw: "", want: 500 * time.Millisecond},
		{name: "blank defaults", raw: "  ", want: 500 * time.Millisecond},
		{name: "override in seconds", raw: "0.05", want: 50 * time.Millisecond},
		{name: "integer seconds", raw: "2", want: 2 * time.Second},
		{name: "invalid number", raw: "fast", wantErr: true},
		{name: "not a number", raw: "NaN", wantErr: true},
		{name: "infinite", raw: "Inf", wantErr: true},
		{name: "zero is rejected", raw: "0", wantErr: true},
		{name: "negative is rejected", raw: "-1", wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			interval, err := documentWorkerPollInterval(testCase.raw)
			if testCase.wantErr {
				if err == nil || !strings.Contains(err.Error(), "RAG_DOCUMENT_WORKER_POLL_SECONDS") {
					t.Fatalf("error = %v, want the variable name", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if interval != testCase.want {
				t.Fatalf("interval = %v, want %v", interval, testCase.want)
			}
		})
	}
}
