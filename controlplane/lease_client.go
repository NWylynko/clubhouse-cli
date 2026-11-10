package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (c *Client) ClaimLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	lease, err := c.leaseCall(ctx, http.MethodPut, "/v1/sessions/"+pin+"/leases/"+req.Service, req)
	if err != nil {
		return lease, err
	}
	c.cacheLease(pin, lease)
	return lease, nil
}

func (c *Client) cacheLease(pin string, lease Lease) {
	if lease.Service == "" {
		return
	}
	c.setCachedLease(pin, lease.Service, &lease)
}

func (c *Client) setCachedLease(pin, service string, lease *Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sess, ok := c.cache[pin]
	if !ok {
		return
	}
	leases := make(map[string]Lease, len(sess.Leases)+1)
	for k, v := range sess.Leases {
		if k == service {
			continue
		}
		leases[k] = v
	}
	if lease != nil {
		leases[service] = *lease
	}
	sess.Leases = leases
	c.cache[pin] = sess
}

func (c *Client) RenewLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	return c.leaseCall(ctx, http.MethodPost, "/v1/sessions/"+pin+"/leases/"+req.Service+"/renew", req)
}

func (c *Client) HandoverLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	lease, err := c.leaseCall(ctx, http.MethodPost, "/v1/sessions/"+pin+"/leases/"+req.Service+"/handover", req)
	if err == nil {
		if lease.Service == "" {
			c.setCachedLease(pin, req.Service, nil)
		} else {
			c.cacheLease(pin, lease)
		}
	}
	return lease, err
}

func (c *Client) ReleaseLease(ctx context.Context, pin, service string) error {
	_, err := c.leaseCall(ctx, http.MethodDelete,
		"/v1/sessions/"+pin+"/leases/"+service, nil)
	if err == nil {
		c.setCachedLease(pin, service, nil)
	}
	return err
}

func (c *Client) ReportStartFailure(ctx context.Context, pin string, req LeaseRequest) error {
	_, err := c.leaseCall(ctx, http.MethodPost,
		"/v1/sessions/"+pin+"/leases/"+req.Service+"/start-failed", req)
	if err == nil {
		c.setCachedLease(pin, req.Service, nil)
	}
	return err
}

func (c *Client) Leases(ctx context.Context, pin string) (map[string]Lease, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return nil, err
	}
	return sess.Leases, nil
}

func (c *Client) leaseCall(ctx context.Context, method, path string, body any) (Lease, error) {
	var out Lease
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return out, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := newVersionedRequest(ctx, method, c.Base+path, rdr)
	if err != nil {
		return out, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.attachBearer(req, path)
	res, err := c.http().Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	switch {
	case res.StatusCode == http.StatusUpgradeRequired:
		return out, upgradeRequired(path, b)
	case res.StatusCode == 409:
		return out, fmt.Errorf("%w: %s", ErrLeaseHeld, strings.TrimSpace(trimLeaseErr(string(b))))
	case res.StatusCode == 204:
		return out, nil
	case res.StatusCode >= 300:
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = res.Status
		}
		return out, fmt.Errorf("%s: %s", path, msg)
	}
	if len(b) == 0 {
		return out, nil
	}
	return out, json.Unmarshal(b, &out)
}

func trimLeaseErr(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), ErrLeaseHeld.Error()+": ")
}

func (c *Client) ReportBoxBootFailure(ctx context.Context, pin, memberID, reason string) error {
	if pin == "" || memberID == "" {
		return fmt.Errorf("box boot failure needs the pin and the box's member id")
	}
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/box/boot-failed",
		BoxBootFailure{Reason: reason}, nil)
}
