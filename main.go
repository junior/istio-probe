// istio-probe — a tiny in-cluster diagnostics page to test & validate Istio after an
// upgrade. Deploy it behind the Istio ingress gateway, open it, and it shows the request
// the mesh forwarded plus a live inventory: counts of every Istio resource type, the
// istiod version + readiness, ingress gateway, mesh mTLS mode, and injected namespaces.
// It reads the Kubernetes API with the pod's ServiceAccount (a read-only ClusterRole) —
// single static binary, standard library only.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/index.html
var files embed.FS

var (
	appVersion = "dev"     // overridden via -ldflags at build time
	buildTime  = "unknown" // overridden via -ldflags at build time
	started    = time.Now()
	tmpl       = template.Must(template.ParseFS(files, "templates/index.html"))
)

// ---- data model ----

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type pageData struct {
	Server    serverInfo  `json:"server"`
	Request   requestInfo `json:"request"`
	Pod       []KV        `json:"pod"`
	Facts     []KV        `json:"facts"`
	Kube      kubeInfo    `json:"kubernetes"`
	Istio     istioInfo   `json:"istio"`
	Demo      bool        `json:"demo"`
	Author    *authorInfo `json:"author,omitempty"`
	Generated string      `json:"generated"`
}

type serverInfo struct {
	Version  string `json:"version"`
	Build    string `json:"build"`
	Go       string `json:"go"`
	Uptime   string `json:"uptime"`
	Hostname string `json:"hostname"`
}

type headerKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Mesh  bool   `json:"mesh"`
}

type requestInfo struct {
	Method     string     `json:"method"`
	Proto      string     `json:"proto"`
	Host       string     `json:"host"`
	URI        string     `json:"uri"`
	RemoteAddr string     `json:"remote_addr"`
	ClientIP   string     `json:"client_ip"`
	Scheme     string     `json:"scheme"`
	RequestID  string     `json:"request_id"`
	ViaMesh    bool       `json:"via_mesh"`
	Headers    []headerKV `json:"headers"`
}

type kubeInfo struct {
	Available  bool   `json:"available"`
	Error      string `json:"error,omitempty"`
	APIServer  string `json:"api_server,omitempty"`
	GitVersion string `json:"git_version,omitempty"`
	Major      string `json:"major,omitempty"`
	Minor      string `json:"minor,omitempty"`
	Platform   string `json:"platform,omitempty"`
	BuildDate  string `json:"build_date,omitempty"`
	GoVersion  string `json:"go_version,omitempty"`
}

type istioInfo struct {
	Available  bool            `json:"available"`
	Error      string          `json:"error,omitempty"`
	Namespace  string          `json:"namespace"`
	Version    string          `json:"version,omitempty"`
	Istiod     string          `json:"istiod,omitempty"`
	IngressGW  string          `json:"ingress_gateway,omitempty"`
	MTLSMode   string          `json:"mtls_mode,omitempty"`
	InjectedNS int             `json:"injected_namespaces"`
	Resources  []istioResource `json:"resources"`
}

type istioResource struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"` // -1 means the count couldn't be read
	Note  string `json:"note,omitempty"`
}

// The Istio resource types we count, in display order.
var istioResources = []struct{ group, plural, kind string }{
	{"networking.istio.io", "virtualservices", "VirtualService"},
	{"networking.istio.io", "gateways", "Gateway"},
	{"networking.istio.io", "destinationrules", "DestinationRule"},
	{"networking.istio.io", "serviceentries", "ServiceEntry"},
	{"networking.istio.io", "sidecars", "Sidecar"},
	{"networking.istio.io", "envoyfilters", "EnvoyFilter"},
	{"networking.istio.io", "workloadentries", "WorkloadEntry"},
	{"networking.istio.io", "workloadgroups", "WorkloadGroup"},
	{"security.istio.io", "peerauthentications", "PeerAuthentication"},
	{"security.istio.io", "authorizationpolicies", "AuthorizationPolicy"},
	{"security.istio.io", "requestauthentications", "RequestAuthentication"},
	{"telemetry.istio.io", "telemetries", "Telemetry"},
	{"extensions.istio.io", "wasmplugins", "WasmPlugin"},
}

// ---- collectors ----

func collect(r *http.Request) pageData {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	d := pageData{
		Server: serverInfo{
			Version:  appVersion,
			Build:    buildTime,
			Go:       runtime.Version(),
			Uptime:   time.Since(started).Round(time.Second).String(),
			Hostname: host,
		},
		Request:   collectRequest(r),
		Pod:       collectPod(),
		Facts:     collectFacts(),
		Generated: time.Now().Format("2006-01-02 15:04:05 MST"),
	}
	if k, err := newK8s(); err != nil {
		d.Kube = kubeInfo{Error: err.Error()}
		d.Istio = istioInfo{Error: err.Error(), InjectedNS: -1}
	} else {
		d.Kube = collectKube(ctx, k)
		d.Istio = collectIstio(ctx, k)
	}
	if brandEnabled() {
		d.Author = &authorInfo{Name: "Adao Oliveira Jr", URL: "https://adao.dev"}
	}
	if demoMode() {
		applyDemo(&d)
	}
	return d
}

func meshHeader(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "x-forwarded") ||
		strings.HasPrefix(n, "x-envoy") ||
		strings.HasPrefix(n, "x-b3-") ||
		n == "x-request-id" || n == "x-real-ip" || n == "forwarded" || n == "via" || n == "b3"
}

func collectRequest(r *http.Request) requestInfo {
	ri := requestInfo{
		Method:     r.Method,
		Proto:      r.Proto,
		Host:       r.Host,
		URI:        r.RequestURI,
		RemoteAddr: r.RemoteAddr,
		Scheme:     "http",
		RequestID:  r.Header.Get("X-Request-Id"),
	}
	names := make([]string, 0, len(r.Header))
	for n := range r.Header {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := meshHeader(n)
		if m {
			ri.ViaMesh = true
		}
		ri.Headers = append(ri.Headers, headerKV{Name: n, Value: strings.Join(r.Header[n], ", "), Mesh: m})
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		ri.Scheme = xfp
	} else if r.TLS != nil {
		ri.Scheme = "https"
	}
	switch {
	case r.Header.Get("X-Forwarded-For") != "":
		ri.ClientIP = strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	case r.Header.Get("X-Real-Ip") != "":
		ri.ClientIP = r.Header.Get("X-Real-Ip")
	default:
		ri.ClientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return ri
}

func collectPod() []KV {
	order := []struct{ env, label string }{
		{"POD_NAME", "Pod"},
		{"POD_NAMESPACE", "Namespace"},
		{"POD_IP", "Pod IP"},
		{"NODE_NAME", "Node"},
		{"POD_SERVICE_ACCOUNT", "Service account"},
	}
	var out []KV
	for _, o := range order {
		if v := os.Getenv(o.env); v != "" {
			out = append(out, KV{o.label, v})
		}
	}
	if h, err := os.Hostname(); err == nil {
		out = append(out, KV{"Hostname", h})
	}
	out = append(out, KV{"Architecture", runtime.GOOS + "/" + runtime.GOARCH})
	return out
}

func collectFacts() []KV {
	var out []KV
	for _, e := range os.Environ() {
		raw, ok := strings.CutPrefix(e, "PROBE_FACT_")
		if !ok {
			continue
		}
		if k, v, ok := strings.Cut(raw, "="); ok && v != "" {
			out = append(out, KV{strings.ReplaceAll(k, "_", " "), v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var (
	kubeMu    sync.Mutex
	kubeCache *kubeInfo
)

func collectKube(ctx context.Context, k *k8s) kubeInfo {
	kubeMu.Lock()
	defer kubeMu.Unlock()
	if kubeCache != nil && kubeCache.Available {
		return *kubeCache
	}
	var v struct {
		Major, Minor, GitVersion, BuildDate, Platform, GoVersion string
	}
	ki := kubeInfo{APIServer: k.base}
	if err := k.get(ctx, "/version", &v); err != nil {
		ki.Error = err.Error()
	} else {
		ki = kubeInfo{
			Available: true, APIServer: k.base,
			Major: v.Major, Minor: v.Minor, GitVersion: v.GitVersion,
			Platform: v.Platform, BuildDate: v.BuildDate, GoVersion: v.GoVersion,
		}
	}
	kubeCache = &ki
	return ki
}

func collectIstio(ctx context.Context, k *k8s) istioInfo {
	ns := envDefault("ISTIO_NAMESPACE", "istio-system")
	out := istioInfo{Namespace: ns, InjectedNS: -1}

	gv := map[string]string{}
	for _, g := range []string{"networking.istio.io", "security.istio.io", "telemetry.istio.io", "extensions.istio.io"} {
		if v, err := k.preferredVersion(ctx, g); err == nil {
			gv[g] = v
		}
	}
	if gv["networking.istio.io"] == "" {
		out.Error = "Istio CRDs not found (networking.istio.io API group absent) — is Istio installed?"
		return out
	}
	out.Available = true

	for _, rsc := range istioResources {
		v := gv[rsc.group]
		if v == "" {
			continue // that API group isn't installed
		}
		n, err := k.count(ctx, rsc.group, v, rsc.plural)
		if err != nil {
			out.Resources = append(out.Resources, istioResource{Kind: rsc.kind, Count: -1, Note: noteFor(err)})
			continue
		}
		out.Resources = append(out.Resources, istioResource{Kind: rsc.kind, Count: n})
	}

	if img, ready, desired, ok := k.deploymentByLabel(ctx, ns, "app=istiod"); ok {
		out.Version = imageTag(img)
		out.Istiod = fmt.Sprintf("%d/%d ready", ready, desired)
	} else {
		out.Istiod = "not found in " + ns
	}
	if _, ready, desired, ok := k.deploymentByLabel(ctx, ns, "app=istio-ingressgateway"); ok {
		out.IngressGW = fmt.Sprintf("%d/%d ready", ready, desired)
	} else {
		out.IngressGW = "not found"
	}
	out.InjectedNS = k.countInjectedNamespaces(ctx)
	out.MTLSMode = k.meshMTLS(ctx, ns, gv["security.istio.io"])
	return out
}

// ---- Kubernetes API client (ServiceAccount token, stdlib only) ----

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type k8s struct {
	base   string
	token  string
	client *http.Client
}

type apiError struct {
	code   int
	status string
}

func (e *apiError) Error() string { return e.status }

func newK8s() (*k8s, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return nil, errors.New("not running in a Kubernetes cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	base := "https://" + net.JoinHostPort(host, envDefault("KUBERNETES_SERVICE_PORT", "443"))
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("no service-account token: %w", err)
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("no cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &k8s{
		base:   base,
		token:  strings.TrimSpace(string(token)),
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (k *k8s) get(ctx context.Context, path string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{code: resp.StatusCode, status: resp.Status}
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (k *k8s) preferredVersion(ctx context.Context, group string) (string, error) {
	var g struct {
		PreferredVersion struct {
			Version string `json:"version"`
		} `json:"preferredVersion"`
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := k.get(ctx, "/apis/"+group, &g); err != nil {
		return "", err
	}
	if g.PreferredVersion.Version != "" {
		return g.PreferredVersion.Version, nil
	}
	if len(g.Versions) > 0 {
		return g.Versions[0].Version, nil
	}
	return "", fmt.Errorf("no served version for %s", group)
}

func (k *k8s) count(ctx context.Context, group, version, plural string) (int, error) {
	total := 0
	path := fmt.Sprintf("/apis/%s/%s/%s?limit=500", group, version, plural)
	for {
		var list struct {
			Items    []json.RawMessage `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		if err := k.get(ctx, path, &list); err != nil {
			return 0, err
		}
		total += len(list.Items)
		if list.Metadata.Continue == "" {
			return total, nil
		}
		path = fmt.Sprintf("/apis/%s/%s/%s?limit=500&continue=%s", group, version, plural, url.QueryEscape(list.Metadata.Continue))
	}
}

func (k *k8s) deploymentByLabel(ctx context.Context, ns, selector string) (image string, ready, desired int, found bool) {
	var list struct {
		Items []struct {
			Spec struct {
				Replicas int `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	path := "/apis/apps/v1/namespaces/" + ns + "/deployments?labelSelector=" + url.QueryEscape(selector)
	if err := k.get(ctx, path, &list); err != nil || len(list.Items) == 0 {
		return "", 0, 0, false
	}
	d := list.Items[0]
	if len(d.Spec.Template.Spec.Containers) > 0 {
		image = d.Spec.Template.Spec.Containers[0].Image
	}
	return image, d.Status.ReadyReplicas, d.Spec.Replicas, true
}

func (k *k8s) countInjectedNamespaces(ctx context.Context) int {
	var list struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/namespaces", &list); err != nil {
		return -1
	}
	n := 0
	for _, ns := range list.Items {
		l := ns.Metadata.Labels
		if l["istio-injection"] == "enabled" || l["istio.io/rev"] != "" {
			n++
		}
	}
	return n
}

func (k *k8s) meshMTLS(ctx context.Context, ns, secVersion string) string {
	if secVersion == "" {
		return "—"
	}
	var pa struct {
		Spec struct {
			Mtls struct {
				Mode string `json:"mode"`
			} `json:"mtls"`
		} `json:"spec"`
	}
	// the mesh-wide default is a PeerAuthentication named "default" in the root namespace
	err := k.get(ctx, fmt.Sprintf("/apis/security.istio.io/%s/namespaces/%s/peerauthentications/default", secVersion, ns), &pa)
	if err != nil {
		return "PERMISSIVE (default)"
	}
	if pa.Spec.Mtls.Mode == "" {
		return "PERMISSIVE (default)"
	}
	return pa.Spec.Mtls.Mode
}

// ---- brand & demo ----

// brandEnabled reports whether to show the "by adao.dev" credit in the footer and
// /api/info. On by default; set PROBE_BRAND=off (or 0/false/no) to remove it — handy
// for internal/corporate deployments.
func brandEnabled() bool {
	switch strings.ToLower(os.Getenv("PROBE_BRAND")) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

type authorInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func demoMode() bool {
	switch strings.ToLower(os.Getenv("PROBE_DEMO")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func applyDemo(d *pageData) {
	d.Demo = true
	if !d.Kube.Available {
		d.Kube = kubeInfo{
			Available: true, APIServer: "https://10.96.0.1:443",
			Major: "1", Minor: "31", GitVersion: "v1.31.4",
			Platform: "linux/amd64", BuildDate: "2025-12-11T20:00:00Z", GoVersion: "go1.23.4",
		}
	}
	if !d.Istio.Available || d.Istio.Error != "" {
		d.Istio = istioInfo{
			Available: true, Namespace: "istio-system", Version: "1.23.2",
			Istiod: "2/2 ready", IngressGW: "2/2 ready", MTLSMode: "STRICT", InjectedNS: 7,
			Resources: []istioResource{
				{Kind: "VirtualService", Count: 42}, {Kind: "Gateway", Count: 5},
				{Kind: "DestinationRule", Count: 31}, {Kind: "ServiceEntry", Count: 12},
				{Kind: "Sidecar", Count: 3}, {Kind: "EnvoyFilter", Count: 8},
				{Kind: "WorkloadEntry", Count: 0}, {Kind: "WorkloadGroup", Count: 0},
				{Kind: "PeerAuthentication", Count: 4}, {Kind: "AuthorizationPolicy", Count: 19},
				{Kind: "RequestAuthentication", Count: 2}, {Kind: "Telemetry", Count: 1},
				{Kind: "WasmPlugin", Count: 0},
			},
		}
	}
}

// ---- helpers ----

func noteFor(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.code {
		case http.StatusForbidden:
			return "no permission"
		case http.StatusNotFound:
			return "n/a"
		}
	}
	return "error"
}

// imageTag pulls the tag out of an image ref, ignoring any digest and the registry port.
func imageTag(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[colon+1:]
	}
	return image
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s reqid=%q", r.Method, r.URL.Path, r.Proto, r.Header.Get("X-Request-Id"))
	})
}

func main() {
	addr := ":" + envDefault("PORT", "8080")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(collect(r))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "index.html", collect(r)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	srv := &http.Server{Addr: addr, Handler: logMiddleware(mux), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("istio-probe %s (built %s) listening on %s", appVersion, buildTime, addr)
	log.Fatal(srv.ListenAndServe())
}
