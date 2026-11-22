package cloudflarecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	httpsScheme          = "https"
	methodNotAllowed     = "method not allowed"
	httpScheme           = "http"
	errorField           = "error"
	cacheTTL             = 10 * time.Minute
	maxOriginBodyBytes   = 16 << 20
	maxControlBodyBytes  = 1 << 20
	readHeaderTimeout    = 5 * time.Second
	defaultPublicPrefix  = "/i/"
	defaultOriginPrefix  = "/i/"
	purgePathPrefix      = "/client/v4/zones/"
	purgePathSuffix      = "/purge_cache"
	cacheStatusHeader    = "Cf-Cache-Status"
	cacheStatusHit       = "HIT"
	cacheStatusMiss      = "MISS"
	defaultOriginTimeout = 15 * time.Second
)

type ZoneRegistration struct {
	ZoneID       string `json:"zone_id"`
	APIToken     string `json:"api_token"`
	Origin       string `json:"origin"`
	PublicPrefix string `json:"public_prefix,omitempty"`
	// OriginPrefix replaces the public prefix when proxying to the origin;
	// "/" maps `{public_prefix}{suffix}` to `{origin}/{suffix}`. Default "/i/".
	OriginPrefix string `json:"origin_prefix,omitempty"`
}

type ZoneRegistrationResponse struct {
	PublicBaseURL string `json:"public_base_url"`
}

type Fault struct {
	ZoneID     string `json:"zone_id"`
	Operation  string `json:"operation"`
	Path       string `json:"path,omitempty"`
	Status     int    `json:"status"`
	Body       string `json:"body,omitempty"`
	Count      int    `json:"count,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
}

type Server struct {
	client *http.Client
	now    func() time.Time

	mu                 sync.Mutex
	zones              map[string]*zone
	inflight           map[string]*flight
	faults             map[string][]Fault
	purgeRequests      map[string]int64
	acknowledgedPurges map[string]int64
	purgeChanged       chan struct{}
}

type zone struct {
	id           string
	apiToken     string
	origin       *url.URL
	publicPrefix string
	originPrefix string
	cache        map[string]entry
}

type entry struct {
	body     []byte
	header   http.Header
	status   int
	storedAt time.Time
}

type flight struct {
	done chan struct{}
}

type CacheEntry struct {
	Key       string    `json:"key"`
	Status    int       `json:"status"`
	StoredAt  time.Time `json:"stored_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewServer(addr string) (*http.Server, error) {
	if addr == "" {
		return nil, errors.New("new Cloudflare cache emulator server: listener address is empty")
	}
	handler := NewHandler()
	if err := registerDefaultZoneFromEnvironment(handler); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}, nil
}

func NewHandler() *Server {
	return &Server{
		client: &http.Client{
			Timeout: defaultOriginTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:                time.Now,
		zones:              make(map[string]*zone),
		inflight:           make(map[string]*flight),
		faults:             make(map[string][]Fault),
		purgeRequests:      make(map[string]int64),
		acknowledgedPurges: make(map[string]int64),
		purgeChanged:       make(chan struct{}),
	}
}

func registerDefaultZoneFromEnvironment(server *Server) error {
	zoneID := os.Getenv("CLOUDFLARE_CACHE_ZONE_ID")
	token := os.Getenv("CLOUDFLARE_CACHE_API_TOKEN")
	origin := os.Getenv("CLOUDFLARE_CACHE_ORIGIN")
	if zoneID == "" && token == "" && origin == "" {
		return nil
	}
	return server.RegisterZone(ZoneRegistration{
		ZoneID:       zoneID,
		APIToken:     token,
		Origin:       origin,
		PublicPrefix: os.Getenv("CLOUDFLARE_CACHE_PUBLIC_PREFIX"),
		OriginPrefix: os.Getenv("CLOUDFLARE_CACHE_ORIGIN_PREFIX"),
	})
}

// pathPrefix applies the default to an empty prefix and requires leading and
// trailing slashes.
func pathPrefix(zoneID, kind, prefix, fallback string) (string, error) {
	if prefix == "" {
		prefix = fallback
	}
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf(
			"register Cloudflare cache zone %q %s prefix %q: expected leading and trailing slash",
			zoneID,
			kind,
			prefix,
		)
	}
	return prefix, nil
}

func (server *Server) RegisterZone(registration ZoneRegistration) error {
	if registration.ZoneID == "" {
		return errors.New("register Cloudflare cache zone: zone ID is empty")
	}
	if registration.APIToken == "" {
		return fmt.Errorf(
			"register Cloudflare cache zone %q: API token is empty",
			registration.ZoneID,
		)
	}
	origin, err := url.Parse(registration.Origin)
	if err != nil {
		return fmt.Errorf(
			"register Cloudflare cache zone %q origin %q: %w",
			registration.ZoneID,
			registration.Origin,
			err,
		)
	}
	if (origin.Scheme != httpScheme && origin.Scheme != httpsScheme) || origin.Host == "" {
		return fmt.Errorf(
			"register Cloudflare cache zone %q origin %q: expected HTTP URL",
			registration.ZoneID,
			registration.Origin,
		)
	}
	publicPrefix, err := pathPrefix(
		registration.ZoneID, "public", registration.PublicPrefix, defaultPublicPrefix,
	)
	if err != nil {
		return err
	}
	originPrefix, err := pathPrefix(
		registration.ZoneID, "origin", registration.OriginPrefix, defaultOriginPrefix,
	)
	if err != nil {
		return err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	server.zones[registration.ZoneID] = &zone{
		id:           registration.ZoneID,
		apiToken:     registration.APIToken,
		origin:       origin,
		publicPrefix: publicPrefix,
		originPrefix: originPrefix,
		cache:        make(map[string]entry),
	}
	return nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/healthz":
		writer.WriteHeader(http.StatusNoContent)
	case request.URL.Path == "/__test__/zones":
		server.handleZones(writer, request)
	case request.URL.Path == "/__test__/cache":
		server.handleCacheControl(writer, request)
	case request.URL.Path == "/__test__/faults":
		server.handleFaults(writer, request)
	case request.URL.Path == "/__test__/requests":
		server.handleRequestCounts(writer, request)
	case strings.HasPrefix(request.URL.Path, purgePathPrefix):
		server.handlePurge(writer, request)
	default:
		server.handleCache(writer, request)
	}
}

func (server *Server) Shutdown(_ context.Context) error {
	server.client.CloseIdleConnections()
	return nil
}

func (server *Server) handleRequestCounts(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeJSON(
			writer,
			http.StatusMethodNotAllowed,
			map[string]string{errorField: methodNotAllowed},
		)
		return
	}
	zoneID := request.URL.Query().Get("zone_id")
	path := request.URL.Query().Get("path")
	key := cacheKey(zoneID, path)
	after := int64(-1)
	if value := request.URL.Query().Get("wait_after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		after = parsed
	}
	var count int64
	for {
		server.mu.Lock()
		count = server.purgeRequests[key]
		acknowledged, changed := server.acknowledgedPurges[key], server.purgeChanged
		server.mu.Unlock()
		if after < 0 || acknowledged > after {
			break
		}
		select {
		case <-request.Context().Done():
			return
		case <-changed:
		}
	}
	writeJSON(writer, http.StatusOK, struct {
		Purge int64 `json:"purge"`
	}{Purge: count})
}

func (server *Server) handleZones(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, cloudflareResponse{Success: false})
		return
	}
	var registration ZoneRegistration
	if err := decodeJSONBody(request, &registration); err != nil {
		writeControlError(writer, err)
		return
	}
	if err := server.RegisterZone(registration); err != nil {
		writeControlError(writer, err)
		return
	}
	scheme := httpScheme
	if request.TLS != nil {
		scheme = httpsScheme
	}
	prefix := registration.PublicPrefix
	if prefix == "" {
		prefix = defaultPublicPrefix
	}
	writeJSON(writer, http.StatusCreated, ZoneRegistrationResponse{
		PublicBaseURL: scheme + "://" + request.Host + prefix,
	})
}

func (server *Server) handleCacheControl(writer http.ResponseWriter, request *http.Request) {
	zoneID := request.URL.Query().Get("zone_id")
	server.mu.Lock()
	defer server.mu.Unlock()
	selected, ok := server.zones[zoneID]
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]string{errorField: "zone not found"})
		return
	}
	switch request.Method {
	case http.MethodGet:
		entries := make([]CacheEntry, 0, len(selected.cache))
		for key, cached := range selected.cache {
			entries = append(entries, CacheEntry{
				Key:       key,
				Status:    cached.status,
				StoredAt:  cached.storedAt,
				ExpiresAt: cached.storedAt.Add(cacheTTL),
			})
		}
		slices.SortFunc(entries, func(left CacheEntry, right CacheEntry) int {
			return strings.Compare(left.Key, right.Key)
		})
		writeJSON(writer, http.StatusOK, entries)
	case http.MethodDelete:
		selected.cache = make(map[string]entry)
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(
			writer,
			http.StatusMethodNotAllowed,
			map[string]string{errorField: methodNotAllowed},
		)
	}
}

func (server *Server) handleFaults(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeJSON(
			writer,
			http.StatusMethodNotAllowed,
			map[string]string{errorField: methodNotAllowed},
		)
		return
	}
	var fault Fault
	if err := decodeJSONBody(request, &fault); err != nil {
		writeControlError(writer, err)
		return
	}
	if fault.ZoneID == "" || (fault.Operation != "purge" && fault.Operation != "origin") {
		writeControlError(
			writer,
			errors.New("fault requires zone_id and purge or origin operation"),
		)
		return
	}
	if fault.Status < 100 || fault.Status > 599 {
		writeControlError(writer, fmt.Errorf("fault status %d is outside HTTP range", fault.Status))
		return
	}
	if fault.Count <= 0 {
		fault.Count = 1
	}
	server.mu.Lock()
	server.faults[faultKey(fault.ZoneID, fault.Operation, fault.Path)] = append(
		server.faults[faultKey(fault.ZoneID, fault.Operation, fault.Path)],
		fault,
	)
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) handlePurge(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, cloudflareResponse{Success: false})
		return
	}
	selected := server.purgeZone(request.URL.Path)
	if selected == nil {
		writeJSON(writer, http.StatusNotFound, cloudflareResponse{Success: false})
		return
	}
	zoneID := selected.id
	if request.Header.Get("Authorization") != "Bearer "+selected.apiToken {
		writeJSON(writer, http.StatusUnauthorized, cloudflareResponse{Success: false})
		return
	}
	var payload struct {
		Files []string `json:"files"`
	}
	if err := decodeJSONBody(request, &payload); err != nil {
		writeJSON(writer, http.StatusBadRequest, cloudflareResponse{Success: false})
		return
	}
	if len(payload.Files) == 0 || len(payload.Files) > 100 {
		writeJSON(writer, http.StatusBadRequest, cloudflareResponse{Success: false})
		return
	}
	paths, err := purgePaths(payload.Files)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, cloudflareResponse{Success: false})
		return
	}
	attempts, ok := server.purgeAttempts(writer, zoneID, paths)
	if !ok {
		return
	}
	server.mu.Lock()
	for key, attempt := range attempts {
		delete(selected.cache, key)
		server.acknowledgedPurges[key] = max(server.acknowledgedPurges[key], attempt)
	}
	close(server.purgeChanged)
	server.purgeChanged = make(chan struct{})
	server.mu.Unlock()
	writeJSON(writer, http.StatusOK, cloudflareResponse{
		Success:  true,
		Errors:   []any{},
		Messages: []any{},
		Result:   nil,
	})
}

func (server *Server) handleCache(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	selected := server.zoneForPath(request.URL.Path)
	if selected == nil {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	key := cacheKey(selected.id, request.URL.Path)
	for {
		cached, status, wait := server.cachedOrFlight(selected, key)
		switch status {
		case cacheStatusHit:
			server.writeCached(writer, request, cached, cacheStatusHit)
			return
		case "WAIT":
			select {
			case <-request.Context().Done():
				writer.WriteHeader(http.StatusGatewayTimeout)
				return
			case <-wait:
				continue
			}
		default:
			server.fill(writer, request, selected, key)
			return
		}
	}
}

func (server *Server) zoneForPath(path string) *zone {
	server.mu.Lock()
	defer server.mu.Unlock()
	var selected *zone
	for _, candidate := range server.zones {
		if strings.HasPrefix(path, candidate.publicPrefix) {
			if selected == nil || len(candidate.publicPrefix) > len(selected.publicPrefix) {
				selected = candidate
			}
		}
	}
	return selected
}

func (server *Server) cachedOrFlight(
	selected *zone,
	key string,
) (entry, string, <-chan struct{}) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if cached, ok := selected.cache[key]; ok {
		if server.now().Before(cached.storedAt.Add(cacheTTL)) {
			return cached, cacheStatusHit, nil
		}
		delete(selected.cache, key)
	}
	if pending, ok := server.inflight[key]; ok {
		return entry{}, "WAIT", pending.done
	}
	server.inflight[key] = &flight{done: make(chan struct{})}
	return entry{}, cacheStatusMiss, nil
}

//nolint:gosec // The control API explicitly registers the local E2E origin.
func (server *Server) fill(
	writer http.ResponseWriter,
	request *http.Request,
	selected *zone,
	key string,
) {
	defer server.finishFlight(key)
	if fault, exists := server.takeFault(selected.id, "origin", request.URL.Path); exists {
		writer.WriteHeader(fault.Status)
		if request.Method != http.MethodHead {
			_, _ = io.WriteString(writer, fault.Body)
		}
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, selected.publicPrefix)
	originURL := strings.TrimRight(selected.origin.String(), "/") + selected.originPrefix + suffix
	if request.URL.RawQuery != "" {
		originURL += "?" + request.URL.RawQuery
	}
	upstreamRequest, err := http.NewRequestWithContext(
		request.Context(),
		http.MethodGet,
		originURL,
		nil,
	)
	if err != nil {
		http.Error(writer, "bad gateway", http.StatusBadGateway)
		return
	}
	response, err := server.client.Do(upstreamRequest)
	if err != nil {
		http.Error(writer, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOriginBodyBytes+1))
	if err != nil || len(body) > maxOriginBodyBytes {
		http.Error(writer, "bad gateway", http.StatusBadGateway)
		return
	}
	cached := entry{
		body:     body,
		header:   response.Header.Clone(),
		status:   response.StatusCode,
		storedAt: server.now(),
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices ||
		response.StatusCode == http.StatusMovedPermanently {
		server.mu.Lock()
		selected.cache[key] = cached
		server.mu.Unlock()
	}
	server.writeCached(writer, request, cached, cacheStatusMiss)
}

func (server *Server) finishFlight(key string) {
	server.mu.Lock()
	pending := server.inflight[key]
	delete(server.inflight, key)
	server.mu.Unlock()
	close(pending.done)
}

func (server *Server) writeCached(
	writer http.ResponseWriter,
	request *http.Request,
	cached entry,
	cacheStatus string,
) {
	copyHeaders(writer.Header(), cached.header)
	writer.Header().Set(cacheStatusHeader, cacheStatus)
	age := max(int(server.now().Sub(cached.storedAt)/time.Second), 0)
	writer.Header().Set("Age", strconv.Itoa(age))
	writer.Header().Set("Content-Length", strconv.Itoa(len(cached.body)))
	writer.WriteHeader(cached.status)
	if request.Method == http.MethodGet {
		_, _ = writer.Write(cached.body)
	}
}

func faultKey(zoneID string, operation string, path string) string {
	return zoneID + "\x00" + operation + "\x00" + path
}

func cacheKey(zoneID string, path string) string {
	return zoneID + "\x00" + path
}

func decodeJSONBody(request *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxControlBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(body) > maxControlBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxControlBodyBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("decode request JSON: trailing value")
	}
	return nil
}

func copyHeaders(destination http.Header, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

type cloudflareResponse struct {
	Success  bool  `json:"success"`
	Errors   []any `json:"errors"`
	Messages []any `json:"messages"`
	Result   any   `json:"result"`
}

func writeControlError(writer http.ResponseWriter, err error) {
	writeJSON(writer, http.StatusBadRequest, map[string]string{errorField: err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, "encode response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func (server *Server) takeFault(zoneID string, operation string, path string) (Fault, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	key := faultKey(zoneID, operation, path)
	queue := server.faults[key]
	if len(queue) == 0 {
		key = faultKey(zoneID, operation, "")
		queue = server.faults[key]
	}
	if len(queue) == 0 {
		return Fault{}, false
	}
	fault := queue[0]
	fault.Count--
	if fault.Count == 0 {
		queue = queue[1:]
	} else {
		queue[0] = fault
	}
	server.faults[key] = queue
	return fault, true
}

func purgePaths(files []string) ([]string, error) {
	paths := make([]string, 0, len(files))
	for _, value := range files {
		purgedURL, err := url.Parse(value)
		if err != nil || purgedURL.Host == "" ||
			(purgedURL.Scheme != httpsScheme && purgedURL.Scheme != httpScheme) {
			return nil, fmt.Errorf("invalid purge URL %q", value)
		}
		paths = append(paths, purgedURL.Path)
	}
	return paths, nil
}

func (server *Server) purgeZone(path string) *zone {
	zoneID, ok := strings.CutPrefix(path, purgePathPrefix)
	if !ok {
		return nil
	}
	zoneID, ok = strings.CutSuffix(zoneID, purgePathSuffix)
	if !ok || zoneID == "" || strings.Contains(zoneID, "/") {
		return nil
	}
	server.mu.Lock()
	selected, exists := server.zones[zoneID]
	server.mu.Unlock()
	if !exists {
		return nil
	}
	return selected
}

func (server *Server) purgeAttempts(
	writer http.ResponseWriter,
	zoneID string,
	paths []string,
) (map[string]int64, bool) {
	attempts := make(map[string]int64)
	for _, path := range paths {
		key := cacheKey(zoneID, path)
		server.mu.Lock()
		server.purgeRequests[key]++
		attempts[key] = server.purgeRequests[key]
		server.mu.Unlock()
		if fault, exists := server.takeFault(zoneID, "purge", path); exists {
			if fault.RetryAfter != "" {
				writer.Header().Set("Retry-After", fault.RetryAfter)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(fault.Status)
			_, _ = io.WriteString(writer, fault.Body)
			return nil, false
		}
	}
	return attempts, true
}
