package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func waitForMetrics(ctx context.Context, client *http.Client, url string, expected []string) (string, error) {
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				return "", fmt.Errorf("read metrics response: %w", readErr)
			}
			if response.StatusCode != http.StatusOK {
				return "", fmt.Errorf("metrics response: status=%d", response.StatusCode)
			}
			body := string(data)
			// GaugeVec and overflow are collected independently. Require every
			// expected series in the same scrape, including all bounded labels.
			complete := true
			scrape := "\n" + body
			for _, line := range expected {
				complete = complete && strings.Contains(scrape, "\n"+line+"\n")
			}
			if complete {
				return body, nil
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("scheduled worker did not publish persisted outcomes: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

type metricsTransport func(*http.Request) (*http.Response, error)

func (f metricsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnitMetricsPollingWaitsForCompleteScrape(t *testing.T) {
	expected := []string{
		"finassist_phone_callback_overflow_requests_24h 1",
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000001",outcome="confirmed"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000002",outcome="pending"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000003",outcome="expired"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000100",outcome="confirmed"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="other",outcome="pending"} 1`,
	}
	full := strings.Join(expected, "\n") + "\n"
	partials := map[string]string{
		"already complete": full,
		"overflow only":    expected[0] + "\n",
		"empty":            "",
		"wrong value":      strings.Replace(full, expected[0], expected[0]+"0", 1),
		"wrong name":       strings.Replace(full, expected[0], "wrong_"+expected[0], 1),
	}
	for _, line := range expected {
		partials["missing "+line] = strings.Replace(full, line+"\n", "", 1)
	}
	for name, partial := range partials {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			calls := 0
			client := &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
				calls++
				body := full
				if calls == 1 {
					body = partial
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			body, err := waitForMetrics(ctx, client, "http://metrics.test/metrics", expected)
			wantCalls := 2
			if partial == full {
				wantCalls = 1
			}
			if err != nil || calls != wantCalls || body != full {
				t.Fatalf("partial scrape accepted or complete scrape lost: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestUnitMetricsPollingDoesNotCombineScrapes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	expected := []string{"metric_a 1", "metric_b 1"}
	calls := 0
	client := &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(expected[calls-1] + "\n"))}, nil
	})}
	if _, err := waitForMetrics(ctx, client, "http://metrics.test/metrics", expected); !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("combined partial scrapes or ignored cancellation: calls=%d err=%v", calls, err)
	}
}

func TestUnitMetricsPollingRetriesTransportErrorsUntilDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	client := &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("listener not ready")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("metric 1\n"))}, nil
	})}
	if _, err := waitForMetrics(ctx, client, "http://metrics.test/metrics", []string{"metric 1"}); err != nil || calls != 2 {
		t.Fatalf("transport retry: calls=%d err=%v", calls, err)
	}
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	client.Transport = metricsTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err := waitForMetrics(expired, client, "http://metrics.test/metrics", []string{"metric 1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not preserved: %v", err)
	}
}

func TestUnitMetricsPollingRejectsInvalidResponses(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
				var reader io.Reader = strings.NewReader("metric 1\n")
				if status == http.StatusOK {
					reader = metricsBrokenReader{}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(reader)}, nil
			})}
			if _, err := waitForMetrics(t.Context(), client, "http://metrics.test/metrics", []string{"metric 1"}); err == nil {
				t.Fatal("accepted failed metrics response")
			}
		})
	}
	if _, err := waitForMetrics(t.Context(), http.DefaultClient, "://", nil); err == nil {
		t.Fatal("accepted invalid URL")
	}
}

type metricsBrokenReader struct{}

func (metricsBrokenReader) Read([]byte) (int, error) { return 0, errors.New("metrics read failed") }
