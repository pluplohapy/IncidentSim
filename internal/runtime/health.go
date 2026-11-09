package runtime

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// waitEndpoint polls one healthcheck endpoint until the service answers or the
// poll budget is exhausted. Both engines share it so that "Up returned" means
// the same thing for compose and for Firecracker.
//
// Any HTTP response counts, including a 5xx: an incident environment is meant
// to come up degraded, and "the process answers" is what makes it usable for a
// diagnosis. Whether the incident is actually fixed is the verifier's job,
// not the adapter's.
func waitEndpoint(ctx context.Context, service Service, check *HealthcheckSpec, budget time.Duration) error {
	client := &http.Client{Timeout: time.Duration(check.TimeoutSeconds) * time.Second}
	deadline := time.Now().Add(budget)
	url := fmt.Sprintf("http://%s%s", service.Endpoint, check.Path)

	var lastErr error
	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("service %s not healthy after %d attempts: %w", service.Name, attempt, lastErr)
			}
			return fmt.Errorf("service %s not healthy after %d attempts", service.Name, attempt)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval(check, attempt)):
		}
	}
}

func interval(check *HealthcheckSpec, attempt int) time.Duration {
	base := time.Duration(check.IntervalSeconds) * time.Second
	if base <= 0 {
		base = time.Second
	}
	if attempt > 10 {
		return base
	}
	return time.Duration(attempt) * base / 5
}
