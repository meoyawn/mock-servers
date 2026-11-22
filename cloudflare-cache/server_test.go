package cloudflarecache

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

//nolint:funlen // Verify caching, HEAD metadata and authenticated purge in one end-to-end lifecycle.
func TestCacheGETHEADQueryKeyAndPurge(t *testing.T) {
	t.Parallel()
	var originRequests atomic.Int64
	origin := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			originRequests.Add(1)
			if request.Method != http.MethodGet {
				t.Errorf("origin method = %s, want GET", request.Method)
			}
			writer.Header().Set("Cache-Control", "public, s-maxage=600")
			_, _ = io.WriteString(writer, "<rss>one</rss>")
		}),
	)
	defer origin.Close()

	handler := NewHandler()
	registerTestZone(t, handler, origin.URL)
	cache := httptest.NewServer(handler)
	defer cache.Close()

	first := request(
		t,
		cache.Client(),
		http.MethodGet,
		cache.URL+"/zones/test/i/lb_one?first=1",
		"",
	)
	defer first.Body.Close()
	assertResponse(t, first, http.StatusOK, cacheStatusMiss, "<rss>one</rss>")
	second := request(
		t,
		cache.Client(),
		http.MethodGet,
		cache.URL+"/zones/test/i/lb_one?second=2",
		"",
	)
	defer second.Body.Close()
	assertResponse(t, second, http.StatusOK, cacheStatusHit, "<rss>one</rss>")
	head := request(t, cache.Client(), http.MethodHead, cache.URL+"/zones/test/i/lb_one", "")
	defer head.Body.Close()
	assertResponse(t, head, http.StatusOK, cacheStatusHit, "")
	length, err := strconv.Atoi(head.Header.Get("Content-Length"))
	if err != nil || length < 10 || length > 30 {
		t.Fatalf(
			"HEAD Content-Length = %q, want a small complete feed",
			head.Header.Get("Content-Length"),
		)
	}
	if originRequests.Load() != 1 {
		t.Fatalf("origin requests = %d, want 1", originRequests.Load())
	}

	purgeBody := `{"files":["` + cache.URL + `/zones/test/i/lb_one"]}`
	purge := request(
		t, cache.Client(),
		http.MethodPost,
		cache.URL+"/client/v4/zones/test-zone/purge_cache",
		purgeBody,
	)
	if purge.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized purge status = %d, want 401", purge.StatusCode)
	}
	_ = purge.Body.Close()
	purge = requestWithAuth(
		t, cache.Client(),
		cache.URL+"/client/v4/zones/test-zone/purge_cache",
		purgeBody,
		"test-token",
	)
	var result cloudflareResponse
	if err := json.NewDecoder(purge.Body).Decode(&result); err != nil {
		t.Fatalf("decode purge response: %v", err)
	}
	_ = purge.Body.Close()
	if purge.StatusCode != http.StatusOK || !result.Success {
		t.Fatalf("purge = status %d success %v, want 200 true", purge.StatusCode, result.Success)
	}

	afterPurge := request(t, cache.Client(), http.MethodGet, cache.URL+"/zones/test/i/lb_one", "")
	defer afterPurge.Body.Close()
	assertResponse(t, afterPurge, http.StatusOK, cacheStatusMiss, "<rss>one</rss>")
}

func TestCachePreservesAndCachesOriginPermanentRedirect(t *testing.T) {
	t.Parallel()
	var originRequests atomic.Int64
	origin := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			originRequests.Add(1)
			writer.Header().Set("Cache-Control", "public, s-maxage=600")
			writer.Header().Set("Location", "https://feeds.example.test/moved.xml")
			writer.WriteHeader(http.StatusMovedPermanently)
		}),
	)
	defer origin.Close()

	handler := NewHandler()
	registerTestZone(t, handler, origin.URL)
	cache := httptest.NewServer(handler)
	defer cache.Close()
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	first := requestWithClient(t, client, http.MethodGet, cache.URL+"/zones/test/i/lb_redirect")
	defer first.Body.Close()
	assertResponse(t, first, http.StatusMovedPermanently, cacheStatusMiss, "")
	if location := first.Header.Get(
		"Location",
	); location != "https://feeds.example.test/moved.xml" {
		t.Fatalf("redirect Location = %q, want replacement feed", location)
	}
	head := requestWithClient(t, client, http.MethodHead, cache.URL+"/zones/test/i/lb_redirect")
	defer head.Body.Close()
	assertResponse(t, head, http.StatusMovedPermanently, cacheStatusHit, "")
	if originRequests.Load() != 1 {
		t.Fatalf("redirect origin requests = %d, want 1", originRequests.Load())
	}
}

//nolint:funlen // Gate the origin and verify concurrent HEAD requests share one completed cache fill.
func TestColdHEADCollapsesConcurrentMissesThroughOriginGET(t *testing.T) {
	t.Parallel()
	var originRequests atomic.Int64
	release := make(chan struct{})
	originStarted := make(chan struct{})
	var started sync.Once
	origin := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			originRequests.Add(1)
			started.Do(func() { close(originStarted) })
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
			_, _ = io.WriteString(writer, "complete")
		}),
	)
	defer origin.Close()

	handler := NewHandler()
	registerTestZone(t, handler, origin.URL)
	cache := httptest.NewServer(handler)
	defer cache.Close()
	releaseOrigin := sync.OnceFunc(func() { close(release) })
	defer releaseOrigin()

	const requestCount = 8
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	responses := make([]*http.Response, requestCount)
	var group errgroup.Group
	defer func() { cancel(); _ = group.Wait() }()
	for index := range requestCount {
		group.Go(func() error {
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodHead,
				cache.URL+"/zones/test/i/lb_collapsed",
				nil,
			)
			if err != nil {
				return err
			}
			response, err := cache.Client().Do(req)
			if err != nil {
				return err
			}
			t.Cleanup(func() { _ = response.Body.Close() })
			responses[index] = response
			return nil
		})
	}
	select {
	case <-originStarted:
	case <-ctx.Done():
		t.Fatal("origin request was not admitted")
	}
	releaseOrigin()
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, response := range responses {
		assertResponse(t, response, http.StatusOK, response.Header.Get(cacheStatusHeader), "")
	}

	if originRequests.Load() != 1 {
		t.Fatalf("origin requests = %d, want 1", originRequests.Load())
	}
}

func TestOriginPrefixReplacesPublicPrefixOnOriginPath(t *testing.T) {
	t.Parallel()
	originPaths := make(chan string, 1)
	origin := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			originPaths <- request.URL.Path
			_, _ = io.WriteString(writer, "<rss>root</rss>")
		}),
	)
	defer origin.Close()

	handler := NewHandler()
	if err := handler.RegisterZone(ZoneRegistration{
		ZoneID:       "root-zone",
		APIToken:     "root-token",
		Origin:       origin.URL + "/bucket/objects",
		PublicPrefix: "/rss/",
		OriginPrefix: "/",
	}); err != nil {
		t.Fatalf("register zone: %v", err)
	}
	cache := httptest.NewServer(handler)
	defer cache.Close()

	response := request(t, cache.Client(), http.MethodGet, cache.URL+"/rss/feed.xml", "")
	defer response.Body.Close()
	assertResponse(t, response, http.StatusOK, cacheStatusMiss, "<rss>root</rss>")
	if path := <-originPaths; path != "/bucket/objects/feed.xml" {
		t.Fatalf("origin path = %q, want /bucket/objects/feed.xml", path)
	}
}

func registerTestZone(t *testing.T, handler *Server, origin string) {
	t.Helper()
	if err := handler.RegisterZone(ZoneRegistration{
		ZoneID:       "test-zone",
		APIToken:     "test-token",
		Origin:       origin,
		PublicPrefix: "/zones/test/i/",
	}); err != nil {
		t.Fatalf("register zone: %v", err)
	}
}

func request(
	t *testing.T,
	client *http.Client,
	method string,
	target string,
	body string,
) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return response
}

func requestWithAuth(
	t *testing.T,
	client *http.Client,
	target string,
	body string,
	token string,
) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		target,
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("new purge request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("purge request: %v", err)
	}
	return response
}

func requestWithClient(
	t *testing.T,
	client *http.Client,
	method string,
	target string,
) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return response
}

func assertResponse(
	t *testing.T,
	response *http.Response,
	status int,
	cacheStatus string,
	body string,
) {
	t.Helper()
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d", response.StatusCode, status)
	}
	if response.Header.Get(cacheStatusHeader) != cacheStatus {
		t.Fatalf(
			"%s = %q, want %q",
			cacheStatusHeader,
			response.Header.Get(cacheStatusHeader),
			cacheStatus,
		)
	}
	actual, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(actual) != body {
		t.Fatalf("body = %q, want %q", actual, body)
	}
}
