package tsapi

import (
	"context"
	"net/url"
)

// Service is a Tailscale Service definition (API v2 /services). Addrs are
// assigned by control on create.
type Service struct {
	Name    string   `json:"name"`
	Addrs   []string `json:"addrs,omitempty"`
	Comment string   `json:"comment,omitempty"`
	Ports   []string `json:"ports,omitempty"` // e.g. "tcp:80"
	Tags    []string `json:"tags,omitempty"`
}

func servicePath(name string) string { return "/services/" + url.PathEscape(name) }

// GetService reads one Service definition. A 404 (IsNotFound) means it
// doesn't exist.
func (c *Client) GetService(ctx context.Context, name string) (Service, error) {
	var s Service
	err := c.do(ctx, "GET", c.tailnetPath(servicePath(name)), nil, &s)
	return s, err
}

// PutService creates or replaces a Service definition and returns it as
// stored (with its VIPs).
func (c *Client) PutService(ctx context.Context, s Service) (Service, error) {
	var out Service
	err := c.do(ctx, "PUT", c.tailnetPath(servicePath(s.Name)), s, &out)
	return out, err
}

// DeleteService deletes a Service definition. Deleting a missing one is not
// an error.
func (c *Client) DeleteService(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", c.tailnetPath(servicePath(name)), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListServices lists the tailnet's Service definitions.
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	var out struct {
		VIPServices []Service `json:"vipServices"`
	}
	err := c.do(ctx, "GET", c.tailnetPath("/services"), nil, &out)
	return out.VIPServices, err
}
