package status

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	group      = "external-dns-openwrt.vizzletf.github.io"
	apiVersion = group + "/v1alpha1"
	kind       = "OpenWrtRecord"
	resource   = "openwrtrecords"

	// fieldManager owns every field server-side apply writes.
	fieldManager = "external-dns-openwrt"
	// managedBy labels the objects this provider writes, so that listing them
	// finds nothing else.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "external-dns-openwrt"

	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Client writes OpenWrtRecord objects to one namespace. It speaks the few
// REST calls it needs itself rather than pulling in client-go.
type Client struct {
	base      string
	namespace string
	tokenFile string
	http      *http.Client
}

// InCluster builds a client from the pod's service account. namespace may be
// empty for the pod's own.
func InCluster(namespace string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}

	if namespace == "" {
		raw, err := os.ReadFile(serviceAccountDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("read the pod namespace, or set PROVIDER_STATUS_NAMESPACE: %w", err)
		}
		namespace = strings.TrimSpace(string(raw))
	}

	return New("https://"+net.JoinHostPort(host, port), serviceAccountDir+"/ca.crt", serviceAccountDir+"/token", namespace)
}

// New builds a client for the API server at base, trusting caFile and
// authenticating with the token in tokenFile.
func New(base, caFile, tokenFile, namespace string) (*Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", caFile)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	return newClient(base, namespace, tokenFile, &http.Client{Transport: transport, Timeout: 10 * time.Second}), nil
}

func newClient(base, namespace, tokenFile string, httpClient *http.Client) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), namespace: namespace, tokenFile: tokenFile, http: httpClient}
}

// object is an OpenWrtRecord as the API server stores it.
type object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec struct {
		DNSName    string   `json:"dnsName"`
		RecordType string   `json:"recordType"`
		Targets    []string `json:"targets"`
	} `json:"spec"`
	Status struct {
		State              string `json:"state"`
		Reason             string `json:"reason,omitempty"`
		LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	} `json:"status"`
}

func (c *Client) collection() string {
	return c.base + "/apis/" + apiVersion + "/namespaces/" + url.PathEscape(c.namespace) + "/" + resource
}

// apply creates or replaces the object with server-side apply. force takes
// the fields over from anyone who edited them by hand.
func (c *Client) apply(ctx context.Context, obj *object) error {
	obj.APIVersion, obj.Kind = apiVersion, kind
	obj.Metadata.Namespace = c.namespace
	obj.Metadata.Labels = map[string]string{managedByLabel: managedBy}

	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	query := url.Values{"fieldManager": {fieldManager}, "force": {"true"}}
	// JSON is YAML, which is what the apply content type asks for.
	return c.do(ctx, http.MethodPatch, c.collection()+"/"+url.PathEscape(obj.Metadata.Name)+"?"+query.Encode(),
		"application/apply-patch+yaml", body, nil)
}

// list returns every object this provider wrote.
func (c *Client) list(ctx context.Context) ([]object, error) {
	var list struct {
		Items []object `json:"items"`
	}
	query := url.Values{"labelSelector": {managedByLabel + "=" + managedBy}}
	if err := c.do(ctx, http.MethodGet, c.collection()+"?"+query.Encode(), "", nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// remove deletes the object. One already gone is not an error.
func (c *Client) remove(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.collection()+"/"+url.PathEscape(name), "", nil, nil)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil
	}
	return err
}

type statusError struct {
	code    int
	message string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("kubernetes API: %d %s: %s", e.code, http.StatusText(e.code), e.message)
}

func (c *Client) do(ctx context.Context, method, uri, contentType string, body []byte, out any) error {
	// Read on every call: the kubelet rotates a projected token in place.
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("read the service account token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, uri, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A refusal comes as a Status object; its message says what was wrong.
		var status struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &status) != nil || status.Message == "" {
			status.Message = strings.TrimSpace(string(raw))
		}
		return &statusError{code: resp.StatusCode, message: status.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
