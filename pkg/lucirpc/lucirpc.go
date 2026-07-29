package lucirpc

//go:generate mockgen -destination=../../internal/mocks/lucirpc/lucirpc.go -package=mocks . LuciRPC

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"go.uber.org/zap"
)

const (
	rpcPath  = "/cgi-bin/luci/rpc/"
	authPath = rpcPath + "auth"
	uciPath  = rpcPath + "uci"
	sysPath  = rpcPath + "sys"

	methodLogin = "login"
)

var (
	ErrRpcLoginFail = errors.New("rpc: login fail")

	ErrHttpUnauthenticated = errors.New("http: Unauthenticated")
	ErrHttpUnauthorized    = errors.New("http: Unauthorized")
	ErrHttpForbidden       = errors.New("http: Forbidden")
)

type LuciRPC interface {
	Uci(context.Context, string, []string) (string, error)
	// Sys calls the rpc/sys endpoint, which exposes the luci.sys Lua module.
	// Used to reload dnsmasq without applying unrelated staged UCI configs.
	Sys(context.Context, string, []string) (string, error)
}

type Payload struct {
	ID     int      `json:"id"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

type Response struct {
	ID     int         `json:"id"`
	Result interface{} `json:"result"`
	Error  interface{} `json:"error"`
}

type lucirpc struct {
	config     *Config
	httpClient *http.Client

	// ExternalDNS drives /records and /records concurrently with the health
	// endpoint, and re-authentication rewrites the token, so guard it.
	mu    sync.RWMutex
	token string
}

func (c *lucirpc) getToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

func (c *lucirpc) setToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

func New(config *Config) (LuciRPC, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: config.InsecureSkipVerify,
			},
			Dial: (&net.Dialer{
				Timeout:   time.Duration(config.Timeout) * time.Second,
				KeepAlive: time.Duration(config.Timeout) * time.Second,
			}).Dial,
		},
	}

	return &lucirpc{
		config:     config,
		httpClient: httpClient,
	}, nil
}

func (c *lucirpc) Uci(ctx context.Context, method string, params []string) (string, error) {
	return c.rpcWithAuth(ctx, uciPath, method, params)
}

func (c *lucirpc) Sys(ctx context.Context, method string, params []string) (string, error) {
	return c.rpcWithAuth(ctx, sysPath, method, params)
}

func (c *lucirpc) auth(ctx context.Context) error {
	token, err := c.rpc(ctx, authPath, methodLogin, []string{c.config.Auth.Username, c.config.Auth.Password})
	if err != nil {
		logger.Log.Error("rpc: login fail", zap.Error(err))
		return err
	}

	// OpenWRT JSON RPC response of wrong username and password
	// {"id":1,"result":null,"error":null}
	if token == "null" {
		return ErrRpcLoginFail
	}

	c.setToken(token)
	return nil
}

func (c *lucirpc) rpc(ctx context.Context, path, method string, params []string) (string, error) {
	data, err := json.Marshal(Payload{
		ID:     c.config.RpcID,
		Method: method,
		Params: params,
	})
	if err != nil {
		logger.Log.Error("marshal fail", zap.Error(err))
		return "", err
	}

	url := c.getUri(path, method)
	respBody, err := c.call(ctx, url, data)
	if err != nil {
		logger.Log.Error("call fail", zap.Error(err))
		return "", err
	}

	var response Response
	if err := json.Unmarshal(respBody, &response); err != nil {
		logger.Log.Error("unmarshal fail", zap.Error(err))
		return "", err
	}

	if response.Error != nil {
		return "", parseError(response.Error)
	}

	if response.Result != nil {
		return parseString(response.Result)
	}

	return "", nil
}

func (c *lucirpc) getUri(path, method string) string {
	// The auth token is a credential — never log it.
	logger.Log.Debug("uri", zap.String("path", path), zap.String("method", method))
	proto := "https://"
	if !c.config.SSL {
		proto = "http://"
	}

	url := proto + c.config.Hostname + ":" + strconv.Itoa(c.config.Port) + path
	if method != methodLogin {
		if token := c.getToken(); token != "" {
			url = url + "?auth=" + token
		}
	}

	return url
}

func (c *lucirpc) call(ctx context.Context, url string, postBody []byte) ([]byte, error) {
	// Neither the URL nor the body may be logged as-is: the URL carries
	// ?auth=<session token> and a login body carries the router password.
	logger.Log.Debug("call", zap.String("url", redactURL(url)))
	body := bytes.NewReader(postBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var respBody []byte
	respBody, err = io.ReadAll(resp.Body)
	if resp.StatusCode > 226 {
		return respBody, c.httpError(resp.StatusCode)
	}

	return respBody, err
}

func (c *lucirpc) httpError(code int) error {
	if code == 401 {
		return ErrHttpUnauthorized
	}

	if code == 403 {
		return ErrHttpForbidden
	}

	return fmt.Errorf("http status code: %d", code)
}

func (c *lucirpc) rpcWithAuth(ctx context.Context, path, method string, params []string) (string, error) {
	result, err := c.rpc(ctx, path, method, params)
	if err == nil {
		return result, nil
	}

	if err != ErrHttpUnauthorized && err != ErrHttpForbidden {
		return "", err
	}

	logger.Log.Info("re-authenticate")
	if err = c.auth(ctx); err != nil {
		return "", err
	}

	return c.rpc(ctx, path, method, params)
}

// redactURL strips the query string, which is where the session token lives.
func redactURL(url string) string {
	if idx := strings.IndexByte(url, '?'); idx >= 0 {
		return url[:idx] + "?<redacted>"
	}
	return url
}

func parseString(obj interface{}) (string, error) {
	if obj == nil {
		return "", errors.New("nil object cannot be parsed")
	}

	var result string
	if _, ok := obj.(string); ok {
		result = fmt.Sprintf("%v", obj)
		return result, nil
	}

	jsonBytes, err := json.Marshal(obj)
	if err == nil {
		result = string(jsonBytes)
	}

	return result, err
}

func parseError(obj interface{}) error {
	result, err := parseString(obj)
	if err != nil {
		return err
	}

	return errors.New(result)
}
