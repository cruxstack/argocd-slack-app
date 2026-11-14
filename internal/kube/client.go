// Package kube is the small read-only Kubernetes client argocd-slack needs:
// list, watch and get over the EKS API with an assumed-role token.
package kube

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Target is one cluster and the role whose access entry can read it; with no
// role the caller's own credentials are used.
type Target struct {
	Cluster string
	Region  string
	RoleARN string
}

// ErrGone means a watch's resourceVersion expired and the caller must relist.
var ErrGone = errors.New("resource version expired")

// Client talks to one cluster's API server.
type Client struct {
	base    string
	http    *http.Client
	presign func(context.Context) (string, error)

	mu      sync.Mutex
	token   string
	minted  time.Time
	tokenTT time.Duration
}

// Connect assumes the target's role, reads the cluster endpoint and CA, and
// returns a client that re-mints its token before STS expires it.
func Connect(ctx context.Context, cfg aws.Config, t Target, session string) (*Client, error) {
	cfg = cfg.Copy()
	cfg.Region = t.Region
	if t.RoleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), t.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = session
		}))
	}

	out, err := eks.NewFromConfig(cfg).DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(t.Cluster)})
	if err != nil {
		return nil, fmt.Errorf("describe cluster %s: %w", t.Cluster, err)
	}
	if out.Cluster == nil || out.Cluster.Endpoint == nil || out.Cluster.CertificateAuthority == nil || out.Cluster.CertificateAuthority.Data == nil {
		return nil, fmt.Errorf("cluster %s has no endpoint or certificate authority", t.Cluster)
	}
	ca, err := base64.StdEncoding.DecodeString(*out.Cluster.CertificateAuthority.Data)
	if err != nil {
		return nil, fmt.Errorf("cluster %s certificate authority: %w", t.Cluster, err)
	}
	stsClient := sts.NewFromConfig(cfg)
	return New(*out.Cluster.Endpoint, ca, func(ctx context.Context) (string, error) {
		return clusterToken(ctx, stsClient, t.Cluster)
	})
}

// New builds a client for an endpoint, trusting only caPEM.
func New(endpoint string, caPEM []byte, presign func(context.Context) (string, error)) (*Client, error) {
	pool := x509.NewCertPool()
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificate in the cluster's certificate authority")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	if len(caPEM) == 0 {
		tr.TLSClientConfig = nil
	}
	return &Client{
		base:    endpoint,
		http:    &http.Client{Transport: tr},
		presign: presign,
		tokenTT: 40 * time.Second,
	}, nil
}

// clusterToken is what `aws eks get-token` prints: a presigned STS
// GetCallerIdentity URL naming the cluster, valid for a minute.
func clusterToken(ctx context.Context, client *sts.Client, cluster string) (string, error) {
	presigned, err := sts.NewPresignClient(client).PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(opts *sts.Options) {
			opts.APIOptions = append(opts.APIOptions,
				smithyhttp.SetHeaderValue("x-k8s-aws-id", cluster),
				smithyhttp.SetHeaderValue("X-Amz-Expires", "60"),
			)
		})
	})
	if err != nil {
		return "", fmt.Errorf("presign cluster token for %s: %w", cluster, err)
	}
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presigned.URL)), nil
}

func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.minted) < c.tokenTT {
		return c.token, nil
	}
	tok, err := c.presign(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.minted = tok, time.Now()
	return tok, nil
}

func (c *Client) request(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusGone {
		resp.Body.Close()
		return nil, ErrGone
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Path: path, Body: string(msg)}
	}
	return resp, nil
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code int
	Path string
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: %d: %s", e.Path, e.Code, e.Body)
}

// Get decodes one GET into out. A 404 returns (false, nil).
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := c.request(cctx, path, q)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	return true, json.NewDecoder(resp.Body).Decode(out)
}

// Text returns a GET body as text, capped at limit bytes (for pod logs).
func (c *Client) Text(ctx context.Context, path string, q url.Values, limit int64) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.request(cctx, path, q)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return string(b), err
}

// List is a Kubernetes list response with raw items.
type List struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []json.RawMessage `json:"items"`
}

// Event is one watch event.
type Event struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// Watch streams events from resourceVersion until timeout or ctx ends. It
// returns the last resourceVersion seen, so the next watch can resume.
func (c *Client) Watch(ctx context.Context, path, resourceVersion string, timeout time.Duration, fn func(Event)) (string, error) {
	q := url.Values{
		"watch":               {"1"},
		"resourceVersion":     {resourceVersion},
		"allowWatchBookmarks": {"true"},
		"timeoutSeconds":      {fmt.Sprint(int(timeout.Seconds()))},
	}
	resp, err := c.request(ctx, path, q)
	if err != nil {
		return resourceVersion, err
	}
	defer resp.Body.Close()

	rv := resourceVersion
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == "ERROR" {
			var st struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(ev.Object, &st)
			if st.Code == http.StatusGone {
				return rv, ErrGone
			}
			return rv, fmt.Errorf("watch %s: error event %s", path, ev.Object)
		}
		var meta struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if json.Unmarshal(ev.Object, &meta) == nil && meta.Metadata.ResourceVersion != "" {
			rv = meta.Metadata.ResourceVersion
		}
		if ev.Type != "BOOKMARK" {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return rv, err
	}
	return rv, nil
}

func isNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}
