package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dovakiin0/proxy-m3u8/config"
	"github.com/dovakiin0/proxy-m3u8/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// Redis client for caching responses
var (
	ctx = config.Ctx
)

// CachedResponse represents a cached response structure
type CachedResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}

const cacheTTL = 1 * time.Hour // Cache expiration time

// generateCacheKey creates a unique cache key that incorporates every request
// condition that can change the upstream result: the target URL and the
// effective Referer header that will be sent upstream. Hashing keeps the key
// compact and safe for arbitrary referer values.
func generateCacheKey(targetURL, effectiveReferer string) string {
	sum := sha256.Sum256([]byte(targetURL + "\x00" + effectiveReferer))
	return "m3u8proxy_cache:v2:" + hex.EncodeToString(sum[:])
}

// resolveUpstreamReferer determines the Referer header value that will actually
// be sent to the upstream server for this request, mirroring the header logic
// used when building the upstream request.
func resolveUpstreamReferer(refererParam, targetOrigin string) string {
	if refererParam != "" {
		return refererParam
	}
	if config.Env.DefaultReferer != "" {
		return config.Env.DefaultReferer
	}
	// Fall back to the target's own origin as a last resort
	return targetOrigin + "/"
}

func M3U8ProxyHandler(c echo.Context) error {
	if config.Client == nil {
		log.Println("Redis client is not initialized. Cache will be ignored.")
	}

	/*
		########################################################################################
		#              Get the target URL and referer from query parameters
		########################################################################################
	*/
	targetURL := c.QueryParam("url")
	if targetURL == "" {
		return c.String(http.StatusBadRequest, "Missing 'url' query parameter")
	}

	if _, err := url.ParseRequestURI(targetURL); err != nil {
		log.Printf("Invalid target URL: %s, error: %v", targetURL, err)
		return c.String(http.StatusBadRequest, "Invalid 'url' query parameter")
	}

	// NOTE: echo already query-decodes parameters exactly once. Using the value
	// as-is avoids double-unescaping referers that legitimately contain
	// '%' or '+' characters.
	refererParam := c.QueryParam("referer")

	// Parse target URL to extract origin
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		log.Printf("Invalid target URL: %s, error: %v", targetURL, err)
		return c.String(http.StatusBadRequest, "Invalid 'url' query parameter")
	}
	targetOrigin := parsedURL.Scheme + "://" + parsedURL.Host

	// The referer actually used for the upstream request. The same value is:
	//  - sent as the Referer header upstream,
	//  - folded into the cache key,
	//  - propagated into rewritten segment/playlist URLs,
	// so cached content always matches the referer it was rewritten for.
	upstreamReferer := resolveUpstreamReferer(refererParam, targetOrigin)

	// Detect resource type from the URL path so query strings don't confuse it
	lowerPath := strings.ToLower(parsedURL.Path)
	isM3U8 := strings.HasSuffix(lowerPath, ".m3u8")
	isOtherStatic := utils.IsStaticFileExtension(targetURL)

	/*
		########################################################################################
		#                          Check cache for existing response
		########################################################################################
	*/
	// if cache exists, we will use it
	cacheKey := generateCacheKey(targetURL, upstreamReferer)
	var cachedData CachedResponse

	if config.IsAvailable && config.Client != nil {
		val, err := config.Client.Get(ctx, cacheKey).Result()
		if err == nil { // Cache hit
			err = json.Unmarshal([]byte(val), &cachedData)
			if err == nil && cachedData.StatusCode >= 200 && cachedData.StatusCode < 300 && len(cachedData.Body) > 0 {
				log.Printf("CACHE HIT for %s (referer: %s)", targetURL, upstreamReferer)
				// Apply cached headers
				for key, values := range cachedData.Headers {
					for _, value := range values {
						c.Response().Header().Add(key, value)
					}
				}
				c.Response().WriteHeader(cachedData.StatusCode)
				_, err = c.Response().Writer.Write(cachedData.Body)
				if err != nil {
					log.Printf("Error writing cached response body for %s: %v", targetURL, err)
				}
				return nil // Served from cache
			}
			if err != nil {
				log.Printf("Error unmarshalling cached data for %s: %v. Fetching from origin.", targetURL, err)
			} else {
				// Defensive: never serve a cached error/empty payload.
				log.Printf("Ignoring unusable cache entry for %s (status: %d). Fetching from origin.", targetURL, cachedData.StatusCode)
			}
			// Proceed to fetch from origin
		} else if err != redis.Nil {
			log.Printf("Redis GET error for key %s: %v. Fetching from origin.", cacheKey, err)
			// Proceed to fetch from origin on other Redis errors
		} else {
			log.Printf("CACHE MISS for %s (referer: %s)", targetURL, upstreamReferer)
		}
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		log.Printf("Error creating request to target %s: %v", targetURL, err)
		return c.String(http.StatusInternalServerError, "Failed to create request to target server")
	}

	// Set browser-like headers to bypass Cloudflare
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Don't set Accept-Encoding manually - let Go's HTTP client handle compression automatically
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Ch-Ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)

	// Send the referer we resolved above; rewritten URLs propagate the same value
	req.Header.Set("Referer", upstreamReferer)
	req.Header.Set("Origin", strings.TrimSuffix(upstreamReferer, "/"))

	upstreamResp, err := utils.ProxyHTTPClient.Do(req)
	if err != nil {
		log.Printf("Error fetching target URL %s: %v", targetURL, err)
		// Nothing is written to the cache on failure paths.
		if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
			return c.String(http.StatusGatewayTimeout, "Upstream server timed out")
		}
		return c.String(http.StatusBadGateway, "Failed to fetch content from upstream server")
	}
	defer upstreamResp.Body.Close()

	rawBodyBytes, err := io.ReadAll(upstreamResp.Body)
	if err != nil {
		log.Printf("Error reading response body from upstream %s: %v", targetURL, err)
		return c.String(http.StatusBadGateway, "Failed to read response from upstream server")
	}

	var responseBodyBytes []byte
	responseHeadersToClient := http.Header{}

	// Whitelist headers to copy
	headerWhitelist := []string{
		"Content-Type", "Content-Disposition", "Accept-Ranges", "Content-Range",
	}
	if upstreamResp.StatusCode == http.StatusOK || upstreamResp.StatusCode == http.StatusPartialContent {
		headerWhitelist = append(headerWhitelist, "ETag", "Last-Modified")
	}

	for _, hName := range headerWhitelist {
		if hVal := upstreamResp.Header.Get(hName); hVal != "" {
			// Add the header to the response headers to be sent to the client
			responseHeadersToClient.Set(hName, hVal)
		}
	}

	// Only playlists are rewritten. Segments (.ts) are binary payloads and must
	// be proxied byte-for-byte; running them through the text transformer would
	// corrupt them.
	if isM3U8 && upstreamResp.StatusCode == http.StatusOK {
		var transformedBodyBuffer bytes.Buffer
		proxyRoutePath := strings.TrimPrefix(c.Path(), "/")
		urlPrefix := proxyRoutePath + "?url="

		// Propagate the exact referer used for this upstream request so nested
		// playlists/segments requested through the proxy keep the same origin,
		// and so their cache keys line up with a direct request using that referer.
		err = utils.ProcessM3U8Stream(bytes.NewReader(rawBodyBytes), &transformedBodyBuffer, targetURL, urlPrefix, upstreamReferer)
		if err != nil {
			log.Printf("Error processing M3U8 stream for %s: %v", targetURL, err)
			return c.String(http.StatusInternalServerError, "Error transforming M3U8 content")
		}
		responseBodyBytes = transformedBodyBuffer.Bytes()
		// Force correct Content-Type for m3u8 playlists regardless of what upstream sent
		responseHeadersToClient.Set("Content-Type", "application/x-mpegURL")
		// Content-Length is not set from upstream as body is transformed
	} else {
		// No transformation or non-OK status
		responseBodyBytes = rawBodyBytes
		// Set Content-Length from upstream if it's a static file or non-OK response and CL is present
		if (isOtherStatic || upstreamResp.StatusCode != http.StatusOK) && upstreamResp.Header.Get("Content-Length") != "" {
			responseHeadersToClient.Set("Content-Length", upstreamResp.Header.Get("Content-Length"))
		}
	}

	/*
		########################################################################################
		#                          Cache the response if Redis is available
		########################################################################################
	*/
	// Only fully successful responses (body completely read, and for playlists
	// the transformation already done above) are eligible for caching. A partial
	// (206) playlist is never cached since it was not fully transformed.
	// Failures never touch the cache, so a failed request can neither poison
	// the key nor evict a good entry written concurrently by another request.
	// The whole entry is written with a single atomic SET, so concurrent
	// writers can only replace a complete entry with another complete entry
	// for the same key.
	cacheable := upstreamResp.StatusCode == http.StatusOK ||
		(upstreamResp.StatusCode == http.StatusPartialContent && !isM3U8)
	if config.IsAvailable && config.Client != nil && cacheable {
		cacheEntry := CachedResponse{
			StatusCode: upstreamResp.StatusCode,
			Headers:    responseHeadersToClient, // Use the headers we decided to send
			Body:       responseBodyBytes,       // This is the (potentially transformed) body
		}
		jsonData, err := json.Marshal(cacheEntry)
		if err != nil {
			log.Printf("Error marshalling data for Redis cache for %s: %v", targetURL, err)
		} else {
			err = config.Client.Set(ctx, cacheKey, jsonData, cacheTTL).Err()
			if err != nil {
				log.Printf("Redis SET error for key %s: %v", cacheKey, err)
			} else {
				log.Printf("CACHED %s (key: %s)", targetURL, cacheKey)
			}
		}
	}

	// Prepare bodyToServe for sending to client
	bodyToServe := bytes.NewReader(responseBodyBytes)
	for key, values := range responseHeadersToClient {
		for _, value := range values {
			c.Response().Header().Set(key, value)
		}
	}

	c.Response().WriteHeader(upstreamResp.StatusCode)

	_, err = io.Copy(c.Response().Writer, bodyToServe)
	if err != nil {
		log.Printf("Error writing response body to client for %s: %v", targetURL, err)
	}

	return nil
}
