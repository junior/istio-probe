package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"docker.io/istio/pilot:1.23.2":                 "1.23.2",
		"gcr.io/istio-release/pilot:1.22.0-distroless": "1.22.0-distroless",
		"private.reg:5000/istio/pilot:1.21.1":          "1.21.1",
		"istio/pilot@sha256:deadbeef":                  "istio/pilot",
		"istio/pilot":                                  "istio/pilot",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMeshHeader(t *testing.T) {
	for _, h := range []string{"X-Request-Id", "X-B3-Traceid", "X-Envoy-Attempt-Count", "X-Forwarded-For", "Forwarded", "Via"} {
		if !meshHeader(h) {
			t.Errorf("meshHeader(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"Accept", "User-Agent", "Content-Type"} {
		if meshHeader(h) {
			t.Errorf("meshHeader(%q) = true, want false", h)
		}
	}
}

func TestCollectRequestMesh(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-Id", "abc-123")
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	ri := collectRequest(r)
	if !ri.ViaMesh {
		t.Error("ViaMesh = false, want true with mesh headers present")
	}
	if ri.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", ri.Scheme)
	}
	if ri.ClientIP != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7 (first XFF hop)", ri.ClientIP)
	}
	if ri.RequestID != "abc-123" {
		t.Errorf("RequestID = %q, want abc-123", ri.RequestID)
	}
}
