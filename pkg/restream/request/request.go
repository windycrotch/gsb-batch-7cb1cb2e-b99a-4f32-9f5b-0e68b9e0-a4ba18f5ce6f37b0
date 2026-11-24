package request

import (
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

type Request struct {
	client    *http.Client
	userAgent string
}

func New(userAgent string) Request {
	return Request{
		client:    &http.Client{},
		userAgent: userAgent,
	}
}

func (r Request) Do(ctx context.Context, requestURL string) (*http.Response, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("request to %s aborted as context cancelled", requestURL)
		default:
			response, err := r.attemptRequest(ctx, requestURL)
			if err != nil || response == nil {
				// Do not keep retrying (or sleep) once the caller gave up.
				if ctx.Err() != nil {
					return nil, fmt.Errorf("request to %s aborted as context cancelled: %w", requestURL, err)
				}
				log.Printf("request to %s failed with error %v. Will retry in 1 second", requestURL, err)
				if err := sleepWithContext(ctx, time.Second); err != nil {
					return nil, fmt.Errorf("request to %s aborted as context cancelled", requestURL)
				}
				continue
			}

			if response.StatusCode < http.StatusBadRequest {
				return response, nil
			}

			if response.StatusCode != http.StatusNotFound {
				if ctx.Err() != nil {
					response.Body.Close()
					return nil, fmt.Errorf("request to %s aborted as context cancelled", requestURL)
				}
				log.Printf("request to %s failed with status code %d. Will retry in 1 second", requestURL, response.StatusCode)
				response.Body.Close()
				if err := sleepWithContext(ctx, time.Second); err != nil {
					return nil, fmt.Errorf("request to %s aborted as context cancelled", requestURL)
				}
				continue
			}

			response.Body.Close()
			return nil, fmt.Errorf("request to %s failed with status code %d", requestURL, response.StatusCode)
		}
	}
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r Request) attemptRequest(context context.Context, requestURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(context, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	request.Header.Add("User-Agent", r.userAgent)
	request.Header.Add("Accept-Encoding", "gzip")

	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}

	if response.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		response.Body = reader
	}

	return response, nil
}

func (r Request) ResolveReference(uri string, referenceURL *url.URL) (*url.URL, error) {
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("cannot parse segment uri %s: %w", uri, err)
	}

	return referenceURL.ResolveReference(parsedURI), nil
}
