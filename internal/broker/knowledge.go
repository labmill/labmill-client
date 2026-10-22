package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"labmill-client/internal/knowledge"
)

// MaxKnowledgeBytes caps what the client reads from a knowledge endpoint.
const MaxKnowledgeBytes = 8 << 20

var (
	// ErrKnowledgeUnavailable means the broker has no knowledge endpoint
	// (404 or 501: an old broker, or one without bundles).
	ErrKnowledgeUnavailable = errors.New("broker: no knowledge endpoint")
	// ErrNotModified means the broker answered 304 to If-None-Match.
	ErrNotModified = errors.New("broker: not modified")
)

// Manifest fetches GET /v1/knowledge/manifest: the broker's current
// know-how version for this user's site and its file list. Same base URL
// and token rules as the messages calls (no redirects, https except
// loopback).
func (c *Client) Manifest(ctx context.Context) (knowledge.Manifest, error) {
	var m knowledge.Manifest
	body, err := c.knowledgeGet(ctx, "/v1/knowledge/manifest", "")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("broker: read manifest: %w", err)
	}
	return m, nil
}

// Bundle fetches GET /v1/knowledge/bundle?version=<version> (the gzip'd tar
// for knowledge.Unmarshal). ifNoneMatch, when not empty, is sent as
// If-None-Match; a 304 returns ErrNotModified.
func (c *Client) Bundle(ctx context.Context, version, ifNoneMatch string) ([]byte, error) {
	return c.knowledgeGet(ctx, "/v1/knowledge/bundle?version="+url.QueryEscape(version), ifNoneMatch)
}

func (c *Client) knowledgeGet(ctx context.Context, path, ifNoneMatch string) ([]byte, error) {
	token, err := c.cfg.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("broker: token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	c.setKnowledgeHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	defer resp.Body.Close()
	c.observe(resp)
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, ErrNotModified
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		return nil, ErrKnowledgeUnavailable
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("broker: %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxKnowledgeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	if len(body) > MaxKnowledgeBytes {
		return nil, fmt.Errorf("broker: %s: answer is larger than %d bytes", path, MaxKnowledgeBytes)
	}
	return body, nil
}
